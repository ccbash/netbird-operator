// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/k8sutil"
	nbv1alpha1ac "github.com/netbirdio/kubernetes-operator/pkg/applyconfigurations/api/v1alpha1"
)

const (
	advertiseAnnotation = "netbird.io/advertise"
	networkAnnotation   = "netbird.io/network"
	zoneAnnotation      = "netbird.io/dns-zone"
	// groupsAnnotation lists the NetBird groups (comma-separated names) the
	// advertised resource joins, so access policies can target it.
	groupsAnnotation = "netbird.io/groups"
	// lbServiceLabel marks the NetworkResource/DNSRecord children advertised for
	// a LoadBalancer Service (value = the Service name), for pruning and lookup.
	lbServiceLabel = "netbird.io/loadbalancer"
)

// LoadBalancerReconciler advertises Service type=LoadBalancer addresses into a
// NetBird network: per LB ingress IP family it owns a NetworkResource (the IP,
// routable via the NetworkRouter peers) and a DNSRecord (<svc>-<ns>.<zone>),
// giving each Service one dualstack name. Whether a Service is advertised
// follows a default-on / namespace-opt-out chain.
type LoadBalancerReconciler struct {
	client.Client

	DefaultAdvertise bool
	// DefaultGroups are the NetBird groups advertised resources join when a
	// Service/namespace sets no netbird.io/groups annotation.
	DefaultGroups []string
	Recorder      record.EventRecorder
}

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

func (r *LoadBalancerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	svc := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var nsObj corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: svc.Namespace}, &nsObj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	advertise := svc.Spec.Type == corev1.ServiceTypeLoadBalancer &&
		svc.DeletionTimestamp.IsZero() &&
		shouldAdvertise(svc, &nsObj, r.DefaultAdvertise)
	if !advertise {
		// type changed away, deleted, or opted out — prune any children.
		return ctrl.Result{}, r.prune(ctx, svc, nil)
	}

	ingress := lbIngressAddresses(svc)
	if len(ingress) == 0 {
		return ctrl.Result{}, nil // waiting for the LB to allocate an address
	}

	network, err := r.resolveNetwork(ctx, svc, &nsObj)
	if err != nil {
		return r.dependencyRequeue(svc, err)
	}
	zone, err := r.resolveZone(ctx, svc, &nsObj)
	if err != nil {
		return r.dependencyRequeue(svc, err)
	}

	ownerRef, err := k8sutil.ControllerReference(svc, r.Scheme())
	if err != nil {
		return ctrl.Result{}, err
	}
	groupRefs := groupReferences(r.resourceGroups(svc, &nsObj))
	logf.FromContext(ctx).V(1).Info("advertising LoadBalancer service", "network", network.Name, "zone", zone.Name)

	desired := map[string]bool{}
	for _, fa := range ingress {
		recordType, ok := dnsRecordTypeFor(fa.address)
		if !ok {
			continue
		}
		name := childName(svc.Name, fa.family)
		desired[name] = true
		labels := map[string]string{lbServiceLabel: svc.Name}

		resourceSpec := nbv1alpha1ac.NetworkResourceSpec().
			WithNetworkRef(nbv1alpha1ac.CrossNamespaceReference().
				WithName(network.Name).WithNamespace(network.Namespace)).
			WithName(fmt.Sprintf("%s-%s-%s", svc.Name, svc.Namespace, fa.family)).
			WithAddress(fa.address).
			WithEnabled(true)
		if len(groupRefs) > 0 {
			resourceSpec = resourceSpec.WithGroups(groupRefs...)
		}
		resourceAC := nbv1alpha1ac.NetworkResource(name, svc.Namespace).
			WithLabels(labels).
			WithOwnerReferences(ownerRef).
			WithSpec(resourceSpec)
		if err := r.Apply(ctx, resourceAC, client.ForceOwnership); err != nil {
			return ctrl.Result{}, err
		}

		recordAC := nbv1alpha1ac.DNSRecord(name, svc.Namespace).
			WithLabels(labels).
			WithOwnerReferences(ownerRef).
			WithSpec(nbv1alpha1ac.DNSRecordSpec().
				WithZoneRef(nbv1alpha1ac.CrossNamespaceReference().
					WithName(zone.Name).WithNamespace(zone.Namespace)).
				WithName(serviceFQDN(svc.Name, svc.Namespace, zone.Spec.Domain)).
				WithType(string(recordType)).
				WithContent(fa.address).
				WithTTL(int(dnsRecordTTL.Seconds())))
		if err := r.Apply(ctx, recordAC, client.ForceOwnership); err != nil {
			return ctrl.Result{}, err
		}
	}

	recordNormalEvent(r.Recorder, svc, reasonAdvertised,
		"advertised over NetBird as %s (network %s)", serviceFQDN(svc.Name, svc.Namespace, zone.Spec.Domain), network.Name)
	return ctrl.Result{RequeueAfter: resyncInterval}, r.prune(ctx, svc, desired)
}

func (r *LoadBalancerReconciler) dependencyRequeue(svc *corev1.Service, err error) (ctrl.Result, error) {
	recordEvent(r.Recorder, svc, reasonDependencyNotReady, "%s", err.Error())
	return ctrl.Result{RequeueAfter: dependencyRetry}, nil
}

// prune deletes the Service's advertised children whose names are no longer
// desired (a family went away, or advertising was turned off — desired nil).
func (r *LoadBalancerReconciler) prune(ctx context.Context, svc *corev1.Service, desired map[string]bool) error {
	sel := client.MatchingLabels{lbServiceLabel: svc.Name}
	inNS := client.InNamespace(svc.Namespace)

	var resources nbv1alpha1.NetworkResourceList
	if err := r.List(ctx, &resources, inNS, sel); err != nil {
		return err
	}
	for i := range resources.Items {
		if !desired[resources.Items[i].Name] {
			if err := r.Delete(ctx, &resources.Items[i]); err != nil && !kerrors.IsNotFound(err) {
				return err
			}
		}
	}

	var records nbv1alpha1.DNSRecordList
	if err := r.List(ctx, &records, inNS, sel); err != nil {
		return err
	}
	for i := range records.Items {
		if !desired[records.Items[i].Name] {
			if err := r.Delete(ctx, &records.Items[i]); err != nil && !kerrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (r *LoadBalancerReconciler) resolveNetwork(ctx context.Context, svc *corev1.Service, ns *corev1.Namespace) (*nbv1alpha1.Network, error) {
	name := annotation(svc, ns, networkAnnotation)
	var list nbv1alpha1.NetworkList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	if name != "" {
		for i := range list.Items {
			if list.Items[i].Name == name {
				return &list.Items[i], nil
			}
		}
		return nil, fmt.Errorf("%w: Network %q not found", errDependencyNotReady, name)
	}
	if len(list.Items) == 1 {
		return &list.Items[0], nil
	}
	return nil, fmt.Errorf("%w: no single Network (found %d); set the %s annotation", errDependencyNotReady, len(list.Items), networkAnnotation)
}

func (r *LoadBalancerReconciler) resolveZone(ctx context.Context, svc *corev1.Service, ns *corev1.Namespace) (*nbv1alpha1.DNSZone, error) {
	name := annotation(svc, ns, zoneAnnotation)
	var list nbv1alpha1.DNSZoneList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	if name != "" {
		for i := range list.Items {
			if list.Items[i].Name == name {
				return &list.Items[i], nil
			}
		}
		return nil, fmt.Errorf("%w: DNSZone %q not found", errDependencyNotReady, name)
	}
	if len(list.Items) == 1 {
		return &list.Items[0], nil
	}
	return nil, fmt.Errorf("%w: no single DNSZone (found %d); set the %s annotation", errDependencyNotReady, len(list.Items), zoneAnnotation)
}

func (r *LoadBalancerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		WithLogConstructor(logConstructor(mgr, "LoadBalancer")).
		Owns(&nbv1alpha1.NetworkResource{}).
		Owns(&nbv1alpha1.DNSRecord{}).
		Complete(r)
}

// shouldAdvertise resolves the advertise decision: Service annotation, then
// namespace annotation, then the operator default.
func shouldAdvertise(svc *corev1.Service, ns *corev1.Namespace, def bool) bool {
	if v, ok := svc.Annotations[advertiseAnnotation]; ok {
		return v == "true"
	}
	if v, ok := ns.Annotations[advertiseAnnotation]; ok {
		return v == "true"
	}
	return def
}

// annotation returns the Service annotation, falling back to the namespace.
func annotation(svc *corev1.Service, ns *corev1.Namespace, key string) string {
	if v, ok := svc.Annotations[key]; ok {
		return v
	}
	return ns.Annotations[key]
}

// resourceGroups resolves the NetBird group names an advertised resource joins:
// the netbird.io/groups annotation (Service > namespace), else the operator
// default. Resources need a group for access policies to target them.
func (r *LoadBalancerReconciler) resourceGroups(svc *corev1.Service, ns *corev1.Namespace) []string {
	v := annotation(svc, ns, groupsAnnotation)
	if v == "" {
		return r.DefaultGroups
	}
	var out []string
	for _, g := range strings.Split(v, ",") {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// groupReferences builds GroupReference-by-name apply configs from group names.
func groupReferences(names []string) []*nbv1alpha1ac.GroupReferenceApplyConfiguration {
	refs := make([]*nbv1alpha1ac.GroupReferenceApplyConfiguration, 0, len(names))
	for _, n := range names {
		refs = append(refs, nbv1alpha1ac.GroupReference().WithName(n))
	}
	return refs
}

// lbIngressAddresses returns the Service's LoadBalancer ingress IPs paired with
// their IP family.
func lbIngressAddresses(svc *corev1.Service) []familyAddress {
	var out []familyAddress
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP == "" {
			continue
		}
		family := ipFamilyOf(ing.IP)
		if family == "" {
			continue
		}
		out = append(out, familyAddress{family: family, address: ing.IP})
	}
	return out
}
