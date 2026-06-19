// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/gatewayutil"
	"github.com/netbirdio/kubernetes-operator/internal/k8sutil"
	nbv1alpha1ac "github.com/netbirdio/kubernetes-operator/pkg/applyconfigurations/api/v1alpha1"
)

// gatewayDNSZoneName is the deterministic name of the DNSZone a Gateway owns
// (the Gateway controller creates it; route controllers resolve it).
func gatewayDNSZoneName(gw *gwv1.Gateway) string { return gw.Name }

// reconcileRouteParent resolves a route's parent Gateway and, when it is
// programmed, ensures the route's reachability objects (NetworkResource +
// DNSRecord per backend/family) exist. Parents that aren't the netbird class
// are ignored; not-ready dependencies requeue with an event rather than error.
func reconcileRouteParent(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	rec record.EventRecorder,
	owner client.Object,
	ownerKind string,
	parent gwv1.ParentReference,
	namespace string,
	backendNames []string,
) (ctrl.Result, error) {
	gw, err := gatewayutil.GetParentGateway(ctx, c, parent, namespace, GatewayControllerName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if gw == nil {
		return ctrl.Result{}, nil
	}
	if !meta.IsStatusConditionTrue(gw.Status.Conditions, string(gwv1.GatewayConditionProgrammed)) {
		recordEvent(rec, owner, reasonDependencyNotReady, "Gateway %s is not programmed yet", gw.Name)
		return ctrl.Result{RequeueAfter: gatewayPoll}, nil
	}

	network, err := gatewayutil.GetGatewayNetwork(ctx, c, gw)
	if kerrors.IsNotFound(err) {
		recordEvent(rec, owner, reasonDependencyNotReady, "Network for Gateway %s not found", gw.Name)
		return ctrl.Result{RequeueAfter: dependencyRetry}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	zone := &nbv1alpha1.DNSZone{}
	err = c.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: gatewayDNSZoneName(gw)}, zone)
	if kerrors.IsNotFound(err) {
		recordEvent(rec, owner, reasonDependencyNotReady, "DNSZone for Gateway %s not found", gw.Name)
		return ctrl.Result{RequeueAfter: dependencyRetry}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	svcIdx, err := collectBackendServices(ctx, c, namespace, backendNames)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := ensureRouteExposure(ctx, c, scheme, owner, ownerKind, network, zone.Name, zone.Namespace, zone.Spec.Domain, svcIdx); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// routeChildLabel marks the NetworkResource/DNSRecord objects a route owns, so
// they can be listed for pruning when backends change.
const routeChildLabel = "netbird.io/route"

func routeChildValue(kind, name string) string { return kind + "." + name }

// childName is the deterministic name of a route child for one Service and IP
// family, e.g. "app-svc-ipv4". One route is assumed per Service (see the
// "one owner per NetworkResource" rule), so the route name is not part of it.
func childName(svcName string, family corev1.IPFamily) string {
	return svcName + "-" + strings.ToLower(string(family))
}

// collectBackendServices fetches the Services named by a route's backendRefs,
// deduplicated by name. Missing Services are skipped (their children are pruned)
// rather than erroring.
func collectBackendServices(ctx context.Context, c client.Client, namespace string, names []string) (map[string]corev1.Service, error) {
	svcIdx := map[string]corev1.Service{}
	for _, name := range names {
		var svc corev1.Service
		err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &svc)
		if kerrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		svcIdx[svc.Name] = svc
	}
	return svcIdx, nil
}

// ensureRouteExposure makes the reachability objects for a route exist: per
// backend Service, per ClusterIP family, one NetworkResource (the ClusterIP
// routed into the network) and one DNSRecord (<svc>-<ns>.<zone> -> ClusterIP).
// It then prunes any route children no longer desired. The route owns every
// child, so deleting the route garbage-collects them (and their finalizers
// clean up the NetBird side).
func ensureRouteExposure(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	owner client.Object,
	ownerKind string,
	network *nbv1alpha1.Network,
	zoneName, zoneNamespace, zoneDomain string,
	svcIdx map[string]corev1.Service,
) error {
	ownerRef, err := k8sutil.ControllerReference(owner, scheme)
	if err != nil {
		return err
	}
	labelVal := routeChildValue(ownerKind, owner.GetName())

	desired := map[string]bool{}
	for _, svc := range svcIdx {
		for _, fa := range familyAddresses(&svc, nil) {
			name := childName(svc.Name, fa.family)
			desired[name] = true
			recordType, ok := dnsRecordTypeFor(fa.address)
			if !ok {
				continue
			}

			resourceAC := nbv1alpha1ac.NetworkResource(name, svc.Namespace).
				WithLabels(map[string]string{routeChildLabel: labelVal}).
				WithOwnerReferences(ownerRef).
				WithSpec(nbv1alpha1ac.NetworkResourceSpec().
					WithNetworkRef(nbv1alpha1ac.CrossNamespaceReference().
						WithName(network.Name).WithNamespace(network.Namespace)).
					WithName(name).
					WithAddress(fa.address).
					WithEnabled(true),
				)
			if err := c.Apply(ctx, resourceAC, client.ForceOwnership); err != nil {
				return err
			}

			recordAC := nbv1alpha1ac.DNSRecord(name, svc.Namespace).
				WithLabels(map[string]string{routeChildLabel: labelVal}).
				WithOwnerReferences(ownerRef).
				WithSpec(nbv1alpha1ac.DNSRecordSpec().
					WithZoneRef(nbv1alpha1ac.CrossNamespaceReference().
						WithName(zoneName).WithNamespace(zoneNamespace)).
					WithName(serviceFQDN(svc.Name, svc.Namespace, zoneDomain)).
					WithType(string(recordType)).
					WithContent(fa.address).
					WithTTL(int(dnsRecordTTL.Seconds())),
				)
			if err := c.Apply(ctx, recordAC, client.ForceOwnership); err != nil {
				return err
			}
		}
	}

	return pruneRouteChildren(ctx, c, owner.GetNamespace(), labelVal, desired)
}

// pruneRouteChildren deletes the route's NetworkResource/DNSRecord children
// whose names are no longer desired (a backend was removed or its Service
// deleted).
func pruneRouteChildren(ctx context.Context, c client.Client, namespace, labelVal string, desired map[string]bool) error {
	sel := client.MatchingLabels{routeChildLabel: labelVal}

	var resources nbv1alpha1.NetworkResourceList
	if err := c.List(ctx, &resources, client.InNamespace(namespace), sel); err != nil {
		return err
	}
	for i := range resources.Items {
		if !desired[resources.Items[i].Name] {
			if err := c.Delete(ctx, &resources.Items[i]); err != nil && !kerrors.IsNotFound(err) {
				return err
			}
		}
	}

	var records nbv1alpha1.DNSRecordList
	if err := c.List(ctx, &records, client.InNamespace(namespace), sel); err != nil {
		return err
	}
	for i := range records.Items {
		if !desired[records.Items[i].Name] {
			if err := c.Delete(ctx, &records.Items[i]); err != nil && !kerrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}
