// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/patch"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/k8sutil"
	"github.com/netbirdio/kubernetes-operator/internal/netbirdutil"
)

type NetworkResourceReconciler struct {
	client.Client

	Netbird  *netbird.Client
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=netbird.io,resources=networkresources,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=netbird.io,resources=networkresources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=netbird.io,resources=networkresources/finalizers,verbs=update

func (r *NetworkResourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	netResource := &nbv1alpha1.NetworkResource{}
	err := r.Get(ctx, req.NamespacedName, netResource)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ctrl.LoggerFrom(ctx).V(1).Info("reconciling network resource")
	sp := patch.NewSerialPatcher(netResource, r.Client)

	if !netResource.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, sp, netResource)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      netResource.Spec.ServiceRef.Name,
			Namespace: netResource.Namespace,
		},
	}
	err = r.Get(ctx, client.ObjectKeyFromObject(svc), svc)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return ctrl.Result{}, r.markNotReady(ctx, sp, netResource, "Referenced Service cannot be found.")
		}
		return ctrl.Result{}, err
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		return ctrl.Result{}, r.markNotReady(ctx, sp, netResource, "Referenced Service is not of type ClusterIP.")
	}
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone {
		return ctrl.Result{}, r.markNotReady(ctx, sp, netResource, "Referenced Service does not have a ClusterIP set.")
	}

	netRouter := &nbv1alpha1.NetworkRouter{
		ObjectMeta: metav1.ObjectMeta{
			Name:      netResource.Spec.NetworkRouterRef.Name,
			Namespace: netResource.Spec.NetworkRouterRef.Namespace,
		},
	}
	err = r.Get(ctx, client.ObjectKeyFromObject(netRouter), netRouter)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return ctrl.Result{}, r.markNotReady(ctx, sp, netResource, "Referenced NetworkRouter cannot be found.")
		}
		return ctrl.Result{}, err
	}
	if netRouter.Status.NetworkID == "" || netRouter.Status.RoutingPeerID == "" {
		return ctrl.Result{}, r.markNotReady(ctx, sp, netResource, "Referenced NetworkRouter is not ready.")
	}

	// Inherit the router's resource groups when the NetworkResource doesn't
	// specify its own, so HTTPRoute-created resources (which set no groups) are
	// still reachable by policy.
	groupRefs := netResource.Spec.Groups
	if len(groupRefs) == 0 {
		groupRefs = netRouter.Spec.ResourceGroups
	}
	groupIDs, err := netbirdutil.GetGroupIDs(ctx, r.Client, r.Netbird, groupRefs, netResource.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}

	controllerutil.AddFinalizer(netResource, k8sutil.Finalizer("networkresource"))

	// Resolve the DNS zone first: the resource is a *domain* resource whose
	// address is the Service FQDN (built from the zone's Domain). A reverse-proxy
	// domain target resolves that FQDN via NetBird DNS (the A/AAAA records below)
	// to a ClusterIP, which is reachable via the router's service-CIDR subnet
	// resource. The resource type (domain) is derived by NetBird from the address.
	zone, err := netbirdutil.GetDNSZoneByName(ctx, r.Netbird, netRouter.Spec.DNSZoneRef.Name)
	if errors.Is(err, netbirdutil.ErrZoneNotFound) {
		// The router's DNS zone hasn't been created yet; treat as a not-ready
		// dependency and retry rather than erroring with a stack trace.
		return ctrl.Result{RequeueAfter: 10 * time.Second}, r.markNotReady(ctx, sp, netResource, "Referenced NetworkRouter DNS zone does not exist yet.")
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	// Single label under the zone: NetBird's managed zone only serves a single
	// label below the apex, so svc and namespace are joined with a hyphen
	// ("<svc>-<ns>.<zone>") rather than as dotted sub-labels
	// ("<svc>.<ns>.<zone>"), which the zone creates but doesn't resolve.
	fqdn := strings.Join([]string{svc.Name + "-" + svc.Namespace, zone.Domain}, ".")

	// RoutingMode selects how the resource is addressed: a host resource at the
	// ClusterIP (ip, the default) or a domain resource at the FQDN (domain).
	address, desiredType := resourceAddressFor(svc, fqdn, netResource.Spec.RoutingMode)

	netReq := api.NetworkResourceRequest{
		// NetBird requires resource names to be unique within a network. A
		// routing-mode switch briefly keeps the old and new resources side by
		// side (create-before-delete), so the name is suffixed with the type to
		// keep the two from colliding.
		Name:        resourceName(netResource.UID, desiredType),
		Description: new(svc.Name + "/" + svc.Namespace),
		Address:     address,
		Enabled:     true,
		Groups:      groupIDs,
	}
	resourceID, staleID, err := r.upsertResource(ctx, netRouter.Status.NetworkID, netResource.Status.ResourceID, netReq, desiredType)
	if err != nil {
		return ctrl.Result{}, err
	}
	netResource.Status.NetworkID = netRouter.Status.NetworkID
	netResource.Status.ResourceID = resourceID
	if staleID != "" {
		netResource.Status.StaleResourceIDs = appendUnique(netResource.Status.StaleResourceIDs, staleID)
		recordEvent(r.Recorder, netResource, corev1.EventTypeNormal, reasonRoutingModeSwitch,
			"Routing mode changed to %q; created resource %s, draining old resource %s once the reverse-proxy is repointed",
			netResource.Spec.RoutingMode, resourceID, staleID)
	}
	// Persist the new resource ID before draining the old one: the HTTPRoute
	// controller repoints the reverse-proxy at this ID, which is what finally lets
	// the stale resource be deleted.
	err = sp.Patch(ctx, netResource)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Drain resources left over from a routing-mode change. They delete only once
	// the proxy has been repointed at the current resource, so any that are still
	// in use are kept and retried on a later reconcile.
	if len(netResource.Status.StaleResourceIDs) > 0 {
		remaining, err := r.drainStaleResources(ctx, netResource.Status.NetworkID, netResource.Status.StaleResourceIDs)
		if err != nil {
			return ctrl.Result{}, err
		}
		netResource.Status.StaleResourceIDs = remaining
		if len(remaining) > 0 {
			recordEvent(r.Recorder, netResource, corev1.EventTypeWarning, reasonAwaitingRelease,
				"Old resource(s) %v still targeted by a reverse-proxy; retrying deletion", remaining)
		}
	}

	// Publish A/AAAA records for the FQDN: one A per IPv4 ClusterIP and one AAAA
	// per IPv6 ClusterIP, so the domain target resolves to the backend.
	if err := r.reconcileDNSRecords(ctx, sp, netResource, zone, fqdn, clusterIPsOf(svc)); err != nil {
		return ctrl.Result{}, err
	}

	conditions.MarkTrue(netResource, nbv1alpha1.ReadyCondition, nbv1alpha1.ReconciledReason, "")
	err = sp.Patch(ctx, netResource, patch.WithStatusObservedGeneration{})
	if err != nil {
		return ctrl.Result{}, err
	}
	// A stale resource from a routing-mode change is still in use by its
	// reverse-proxy; retry the drain soon rather than waiting for the resync.
	if len(netResource.Status.StaleResourceIDs) > 0 {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	// Re-reconcile periodically so a resource or DNS record deleted out of band
	// on the NetBird control plane is detected and recreated without waiting for
	// the controller's (multi-hour) resync.
	return ctrl.Result{RequeueAfter: 15 * time.Minute}, nil
}

// markNotReady records a not-ready dependency condition on the NetworkResource
// and patches its status.
func (r *NetworkResourceReconciler) markNotReady(ctx context.Context, sp *patch.SerialPatcher, netResource *nbv1alpha1.NetworkResource, msg string) error {
	conditions.MarkFalse(netResource, nbv1alpha1.ReadyCondition, nbv1alpha1.DependencyReason, "%s", msg)
	recordEvent(r.Recorder, netResource, corev1.EventTypeWarning, reasonDependencyNotReady, "%s", msg)
	return sp.Patch(ctx, netResource)
}

// upsertResource creates or updates the NetBird network resource for netReq,
// returning its ID and, for a routing-mode switch, the ID of the now-stale
// resource the caller must drain. It reads the resource first and acts on the
// result, so a resource that was deleted out of band becomes a clean create
// rather than a failing PUT, and a routing-mode switch (host<->domain) is
// recreated rather than updated: NetBird derives the type from the address but
// won't change it on update, and the proxy target rejects a stale type.
//
// The switch is create-before-delete: NetBird won't delete a resource that a
// reverse-proxy still targets, so deleting the old resource first would
// deadlock (the proxy is only repointed once the new resource's ID is
// published). Instead the new resource is created up front — it has a different
// address and a type-suffixed name, so it coexists with the old one — and the
// old ID is returned for the caller to drain after the proxy moves over.
func (r *NetworkResourceReconciler) upsertResource(ctx context.Context, networkID, existingID string, netReq api.NetworkResourceRequest, desiredType api.NetworkResourceType) (newID, staleID string, err error) {
	resources := r.Netbird.Networks.Resources(networkID)
	if existingID != "" {
		existing, err := resources.Get(ctx, existingID)
		switch {
		case err == nil && existing.Type == desiredType:
			netResp, err := resources.Update(ctx, existingID, netReq)
			if err != nil {
				return "", "", err
			}
			return netResp.Id, "", nil
		case err == nil:
			// Routing mode changed: create the new-typed resource, then hand back
			// the old ID so it is drained once the proxy has been repointed.
			netResp, err := resources.Create(ctx, netReq)
			if err != nil {
				return "", "", err
			}
			return netResp.Id, existingID, nil
		case !netbird.IsNotFound(err):
			return "", "", err
		}
		// Not found (deleted out of band) — fall through to create.
	}
	netResp, err := resources.Create(ctx, netReq)
	if err != nil {
		return "", "", err
	}
	return netResp.Id, "", nil
}

// drainStaleResources deletes resources left over from a routing-mode change and
// returns the IDs that still could not be deleted because a reverse-proxy
// continues to target them (NetBird answers 412 Precondition Failed). Those are
// retried on a later reconcile, once the HTTPRoute controller has repointed the
// proxy at the current resource.
func (r *NetworkResourceReconciler) drainStaleResources(ctx context.Context, networkID string, ids []string) ([]string, error) {
	resources := r.Netbird.Networks.Resources(networkID)
	var remaining []string
	for _, id := range ids {
		err := resources.Delete(ctx, id)
		switch {
		case err == nil, netbird.IsNotFound(err):
			// Drained (or already gone).
		case netbirdutil.IsConflict(err):
			ctrl.LoggerFrom(ctx).Info("stale network resource still in use by a reverse-proxy; awaiting release before deletion", "resourceID", id)
			remaining = append(remaining, id)
		default:
			return nil, err
		}
	}
	return remaining, nil
}

// resourceName builds a NetBird resource name that is unique per routing-mode
// type, so the old and new resources can coexist during a create-before-delete
// routing-mode switch.
func resourceName(uid types.UID, t api.NetworkResourceType) string {
	return string(uid) + "-" + string(t)
}

// appendUnique appends id to ids unless it is already present.
func appendUnique(ids []string, id string) []string {
	for _, existing := range ids {
		if existing == id {
			return ids
		}
	}
	return append(ids, id)
}

// resourceAddressFor returns the NetworkResource address and type for a Service
// under the given routing mode: a host resource at the ClusterIP (ip, the
// default) or a domain resource at the FQDN (domain).
func resourceAddressFor(svc *corev1.Service, fqdn string, mode nbv1alpha1.RoutingMode) (string, api.NetworkResourceType) {
	if mode == nbv1alpha1.RoutingModeDomain {
		return fqdn, api.NetworkResourceTypeDomain
	}
	return svc.Spec.ClusterIP, api.NetworkResourceTypeHost
}

// dnsRecordTypeFor classifies an IP string as an A (IPv4) or AAAA (IPv6) record.
// ok is false when the string is not a valid IP and should be skipped.
func dnsRecordTypeFor(ip string) (api.DNSRecordType, bool) {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "", false
	}
	if parsed.To4() != nil {
		return api.DNSRecordTypeA, true
	}
	return api.DNSRecordTypeAAAA, true
}

// clusterIPsOf returns the Service's dualstack ClusterIPs, falling back to the
// single ClusterIP for older API objects.
func clusterIPsOf(svc *corev1.Service) []string {
	if len(svc.Spec.ClusterIPs) > 0 {
		return svc.Spec.ClusterIPs
	}
	return []string{svc.Spec.ClusterIP}
}

// recordMatchKey builds a comparison key for a DNS record that is stable across
// the multiple textual forms of an IP. An IPv6 address has several
// representations (e.g. "2001:db8::1" vs "2001:0db8:0:0:0:0:0:1"); if NetBird
// stores a record in a different canonical form than the Service's ClusterIP
// string, a raw-string compare would miss the match and the record would be
// deleted and recreated (hitting "identical record already exists"). Comparing
// the canonicalized IP avoids that.
func recordMatchKey(recordType, content string) string {
	if ip := net.ParseIP(content); ip != nil {
		content = ip.String()
	}
	return recordType + "|" + content
}

// reconcileDNSRecords ensures the zone holds one A record per IPv4 and one AAAA
// per IPv6 ClusterIP at fqdn. It reconciles against the zone's *live* records
// (via ListRecords), adopting any that already exist by name+type+content, so a
// status that has drifted from NetBird can't cause a duplicate create
// ("identical record already exists") or a spurious delete. Only stale records
// at this exact fqdn are removed; records under other names are untouched.
func (r *NetworkResourceReconciler) reconcileDNSRecords(ctx context.Context, sp *patch.SerialPatcher, netResource *nbv1alpha1.NetworkResource, zone api.Zone, fqdn string, clusterIPs []string) error {
	type desiredRecord struct {
		rType   api.DNSRecordType
		content string
	}
	var desired []desiredRecord
	for _, ip := range clusterIPs {
		rType, ok := dnsRecordTypeFor(ip)
		if !ok {
			continue
		}
		desired = append(desired, desiredRecord{rType, ip})
	}

	// On a zone change, drop records tracked in the old zone first.
	if netResource.Status.DNSZoneID != "" && netResource.Status.DNSZoneID != zone.Id {
		for _, rec := range netResource.Status.DNSRecords {
			if err := r.Netbird.DNSZones.DeleteRecord(ctx, netResource.Status.DNSZoneID, rec.ID); err != nil && !netbird.IsNotFound(err) {
				return err
			}
		}
		if netResource.Status.DNSRecordID != "" {
			if err := r.Netbird.DNSZones.DeleteRecord(ctx, netResource.Status.DNSZoneID, netResource.Status.DNSRecordID); err != nil && !netbird.IsNotFound(err) {
				return err
			}
		}
		netResource.Status.DNSRecords = nil
		netResource.Status.DNSRecordID = ""
		netResource.Status.DNSZoneID = ""
	}

	// Clean up the legacy single A record (its name used the zone identifier,
	// not the domain) now that records are managed as a set under fqdn.
	if netResource.Status.DNSRecordID != "" {
		if err := r.Netbird.DNSZones.DeleteRecord(ctx, zone.Id, netResource.Status.DNSRecordID); err != nil && !netbird.IsNotFound(err) {
			return err
		}
		netResource.Status.DNSRecordID = ""
	}

	// Index the zone's live records that belong to this resource (name == fqdn),
	// so we can adopt existing ones rather than creating duplicates.
	zoneRecords, err := r.Netbird.DNSZones.ListRecords(ctx, zone.Id)
	if err != nil {
		return err
	}
	existing := map[string]api.DNSRecord{}
	var ours []api.DNSRecord
	for _, rec := range zoneRecords {
		if rec.Name != fqdn {
			continue
		}
		ours = append(ours, rec)
		existing[recordMatchKey(string(rec.Type), rec.Content)] = rec
	}

	kept := make([]nbv1alpha1.DNSRecordStatus, 0, len(desired))
	desiredKeys := map[string]bool{}
	for _, d := range desired {
		key := recordMatchKey(string(d.rType), d.content)
		desiredKeys[key] = true
		if cur, ok := existing[key]; ok {
			kept = append(kept, nbv1alpha1.DNSRecordStatus{Type: string(d.rType), Content: d.content, ID: cur.Id})
			continue
		}
		resp, err := r.Netbird.DNSZones.CreateRecord(ctx, zone.Id, api.DNSRecordRequest{
			Content: d.content,
			Name:    fqdn,
			Ttl:     int(5 * time.Minute / time.Second),
			Type:    d.rType,
		})
		if err != nil {
			return err
		}
		kept = append(kept, nbv1alpha1.DNSRecordStatus{Type: string(d.rType), Content: d.content, ID: resp.Id})
	}

	// Delete stale records at this fqdn (e.g. a previous ClusterIP).
	for _, rec := range ours {
		if !desiredKeys[recordMatchKey(string(rec.Type), rec.Content)] {
			if err := r.Netbird.DNSZones.DeleteRecord(ctx, zone.Id, rec.Id); err != nil && !netbird.IsNotFound(err) {
				return err
			}
		}
	}

	netResource.Status.DNSZoneID = zone.Id
	netResource.Status.DNSRecords = kept
	return sp.Patch(ctx, netResource)
}

func (r *NetworkResourceReconciler) reconcileDelete(ctx context.Context, sp *patch.SerialPatcher, netResource *nbv1alpha1.NetworkResource) (ctrl.Result, error) {
	if netResource.Status.NetworkID != "" && netResource.Status.ResourceID != "" {
		err := r.Netbird.Networks.Resources(netResource.Status.NetworkID).Delete(ctx, netResource.Status.ResourceID)
		if err != nil && !netbird.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	// Also delete any resources still draining from a routing-mode switch, so a
	// resource deleted mid-drain doesn't orphan the old NetBird resource.
	if netResource.Status.NetworkID != "" {
		for _, id := range netResource.Status.StaleResourceIDs {
			err := r.Netbird.Networks.Resources(netResource.Status.NetworkID).Delete(ctx, id)
			if err != nil && !netbird.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}
	if netResource.Status.DNSZoneID != "" {
		for _, rec := range netResource.Status.DNSRecords {
			if err := r.Netbird.DNSZones.DeleteRecord(ctx, netResource.Status.DNSZoneID, rec.ID); err != nil && !netbird.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		if netResource.Status.DNSRecordID != "" {
			if err := r.Netbird.DNSZones.DeleteRecord(ctx, netResource.Status.DNSZoneID, netResource.Status.DNSRecordID); err != nil && !netbird.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}

	controllerutil.RemoveFinalizer(netResource, k8sutil.Finalizer("networkresource"))
	err := sp.Patch(ctx, netResource)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *NetworkResourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := mgr.GetFieldIndexer().IndexField(context.Background(), &nbv1alpha1.NetworkResource{}, ".spec.networkRouterRef", func(obj client.Object) []string {
		netResource := obj.(*nbv1alpha1.NetworkResource)
		ref := netResource.Spec.NetworkRouterRef
		if ref.Name == "" {
			return nil
		}
		if ref.Namespace == "" {
			ref.Namespace = netResource.Namespace
		}
		return []string{fmt.Sprintf("%s/%s", ref.Name, ref.Namespace)}
	})
	if err != nil {
		return err
	}
	err = mgr.GetFieldIndexer().IndexField(context.Background(), &nbv1alpha1.NetworkResource{}, ".spec.serviceRef", func(obj client.Object) []string {
		netResource := obj.(*nbv1alpha1.NetworkResource)
		ref := netResource.Spec.ServiceRef
		if ref.Name == "" {
			return nil
		}
		return []string{netResource.Spec.ServiceRef.Name}
	})
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&nbv1alpha1.NetworkResource{}).
		WithLogConstructor(logConstructor(mgr, "NetworkResource")).
		Watches(
			&nbv1alpha1.NetworkRouter{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				netResourceList := &nbv1alpha1.NetworkResourceList{}
				err := r.List(ctx, netResourceList, client.MatchingFields{".spec.networkRouterRef": fmt.Sprintf("%s/%s", obj.GetName(), obj.GetNamespace())})
				if err != nil {
					return nil
				}

				requests := make([]reconcile.Request, len(netResourceList.Items))
				for i, item := range netResourceList.Items {
					requests[i] = reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name:      item.Name,
							Namespace: item.Namespace,
						},
					}
				}
				return requests
			}),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Watches(
			&corev1.Service{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				netResourceList := &nbv1alpha1.NetworkResourceList{}
				err := r.List(ctx, netResourceList, client.InNamespace(obj.GetNamespace()), client.MatchingFields{".spec.serviceRef": obj.GetName()})
				if err != nil {
					return nil
				}

				requests := make([]reconcile.Request, len(netResourceList.Items))
				for i, item := range netResourceList.Items {
					requests[i] = reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name:      item.Name,
							Namespace: item.Namespace,
						},
					}
				}
				return requests
			}),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Complete(r)
}
