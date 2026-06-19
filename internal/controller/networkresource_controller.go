// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

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

	zone, err := netbirdutil.GetDNSZoneByName(ctx, r.Netbird, netRouter.Spec.DNSZoneRef.Name)
	if errors.Is(err, netbirdutil.ErrZoneNotFound) {
		// The router's DNS zone hasn't been created yet; treat as a not-ready
		// dependency and retry rather than erroring with a stack trace.
		return ctrl.Result{RequeueAfter: dependencyRetry}, r.markNotReady(ctx, sp, netResource, "Referenced NetworkRouter DNS zone does not exist yet.")
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	fqdn := serviceFQDN(svc.Name, svc.Namespace, zone.Domain)

	// Expose one NetBird host resource per ClusterIP family (filtered by
	// spec.IPFamilies), so a dualstack Service is reachable over both.
	addrs := familyAddresses(svc, netResource.Spec.IPFamilies)
	if len(addrs) == 0 {
		return ctrl.Result{}, r.markNotReady(ctx, sp, netResource, "Service has no ClusterIP in the requested IP families.")
	}

	networkID := netRouter.Status.NetworkID
	resources := r.Netbird.Networks.Resources(networkID)
	existingByFamily := map[string]string{}
	for _, e := range netResource.Status.Resources {
		existingByFamily[e.Family] = e.ResourceID
	}

	desc := svc.Name + "/" + svc.Namespace
	entries := make([]nbv1alpha1.NetworkResourceEntry, 0, len(addrs))
	desired := map[string]bool{}
	for _, fa := range addrs {
		family := string(fa.family)
		desired[family] = true
		netReq := api.NetworkResourceRequest{
			// Names are unique within a network, so suffix with the family to let
			// the IPv4 and IPv6 host resources coexist.
			Name:        familyResourceName(netResource.UID, fa.family),
			Description: new(desc),
			Address:     fa.address,
			Enabled:     true,
			Groups:      groupIDs,
		}
		id, err := upsertHostResource(ctx, resources, existingByFamily[family], netReq)
		if err != nil {
			return ctrl.Result{}, err
		}
		entries = append(entries, nbv1alpha1.NetworkResourceEntry{Family: family, Address: fa.address, ResourceID: id})
	}

	// Delete resources for families no longer exposed.
	for _, e := range netResource.Status.Resources {
		if desired[e.Family] {
			continue
		}
		if err := resources.Delete(ctx, e.ResourceID); err != nil && !netbird.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	netResource.Status.NetworkID = networkID
	netResource.Status.Resources = entries
	if err := sp.Patch(ctx, netResource); err != nil {
		return ctrl.Result{}, err
	}

	// Publish A/AAAA records for the exposed addresses.
	if err := r.reconcileDNSRecords(ctx, sp, netResource, zone, fqdn, addressList(addrs)); err != nil {
		return ctrl.Result{}, err
	}

	conditions.MarkTrue(netResource, nbv1alpha1.ReadyCondition, nbv1alpha1.ReconciledReason, "")
	if err := sp.Patch(ctx, netResource, patch.WithStatusObservedGeneration{}); err != nil {
		return ctrl.Result{}, err
	}
	// Re-reconcile periodically so a resource or DNS record deleted out of band
	// on the NetBird control plane is detected and recreated without waiting for
	// the controller's (multi-hour) resync.
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// markNotReady records a not-ready dependency condition on the NetworkResource
// and patches its status.
func (r *NetworkResourceReconciler) markNotReady(ctx context.Context, sp *patch.SerialPatcher, netResource *nbv1alpha1.NetworkResource, msg string) error {
	conditions.MarkFalse(netResource, nbv1alpha1.ReadyCondition, nbv1alpha1.DependencyReason, "%s", msg)
	recordEvent(r.Recorder, netResource, reasonDependencyNotReady, "%s", msg)
	return sp.Patch(ctx, netResource)
}

// upsertHostResource creates or updates a host NetBird resource for netReq. It
// reads the existing resource first, so one deleted out of band becomes a clean
// create rather than a failing update.
func upsertHostResource(ctx context.Context, resources *netbird.NetworkResourcesAPI, existingID string, netReq api.NetworkResourceRequest) (string, error) {
	if existingID != "" {
		if _, err := resources.Get(ctx, existingID); err == nil {
			netResp, err := resources.Update(ctx, existingID, netReq)
			if err != nil {
				return "", err
			}
			return netResp.Id, nil
		} else if !netbird.IsNotFound(err) {
			return "", err
		}
		// Not found (deleted out of band) — fall through to create.
	}
	netResp, err := resources.Create(ctx, netReq)
	if err != nil {
		return "", err
	}
	return netResp.Id, nil
}

// familyResourceName builds a per-family NetBird resource name (e.g.
// "<uid>-ipv4") so the IPv4 and IPv6 host resources for a Service coexist —
// names are unique within a network.
func familyResourceName(uid types.UID, family corev1.IPFamily) string {
	return string(uid) + "-" + strings.ToLower(string(family))
}

// familyAddress pairs a Service ClusterIP with its IP family.
type familyAddress struct {
	family  corev1.IPFamily
	address string
}

// familyAddresses returns the Service's ClusterIPs paired with their IP family,
// filtered to want (all families when want is empty).
func familyAddresses(svc *corev1.Service, want []corev1.IPFamily) []familyAddress {
	wanted := map[corev1.IPFamily]bool{}
	for _, f := range want {
		wanted[f] = true
	}
	var out []familyAddress
	for _, ip := range clusterIPsOf(svc) {
		family := ipFamilyOf(ip)
		if family == "" {
			continue
		}
		if len(want) > 0 && !wanted[family] {
			continue
		}
		out = append(out, familyAddress{family: family, address: ip})
	}
	return out
}

// addressList extracts the addresses from a slice of familyAddress.
func addressList(fas []familyAddress) []string {
	out := make([]string, 0, len(fas))
	for _, fa := range fas {
		out = append(out, fa.address)
	}
	return out
}

// ipFamilyOf classifies an IP string as IPv4 or IPv6, or "" when it is not a
// valid IP.
func ipFamilyOf(ip string) corev1.IPFamily {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if parsed.To4() != nil {
		return corev1.IPv4Protocol
	}
	return corev1.IPv6Protocol
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
	// On a zone change, drop records tracked in the old zone first.
	if netResource.Status.DNSZoneID != "" && netResource.Status.DNSZoneID != zone.Id {
		for _, rec := range netResource.Status.DNSRecords {
			if err := r.Netbird.DNSZones.DeleteRecord(ctx, netResource.Status.DNSZoneID, rec.ID); err != nil && !netbird.IsNotFound(err) {
				return err
			}
		}
		netResource.Status.DNSRecords = nil
		netResource.Status.DNSZoneID = ""
	}

	// Reconcile the records against the live zone (adopt/create/delete) via the
	// shared stateless helper, then record what's present in status.
	kept, err := reconcileZoneRecords(ctx, r.Netbird, zone.Id, fqdn, clusterIPs)
	if err != nil {
		return err
	}

	records := make([]nbv1alpha1.DNSRecordStatus, 0, len(kept))
	for _, rec := range kept {
		records = append(records, nbv1alpha1.DNSRecordStatus{Type: string(rec.Type), Content: rec.Content, ID: rec.Id})
	}
	netResource.Status.DNSZoneID = zone.Id
	netResource.Status.DNSRecords = records
	return sp.Patch(ctx, netResource)
}

func (r *NetworkResourceReconciler) reconcileDelete(ctx context.Context, sp *patch.SerialPatcher, netResource *nbv1alpha1.NetworkResource) (ctrl.Result, error) {
	if netResource.Status.NetworkID != "" {
		for _, e := range netResource.Status.Resources {
			if err := r.Netbird.Networks.Resources(netResource.Status.NetworkID).Delete(ctx, e.ResourceID); err != nil && !netbird.IsNotFound(err) {
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
