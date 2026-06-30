// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1ac "sigs.k8s.io/gateway-api/applyconfiguration/apis/v1"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/k8sutil"
	nbv1alpha1ac "github.com/netbirdio/kubernetes-operator/pkg/applyconfigurations/api/v1alpha1"
)

// gatewayController is the GatewayClass controllerName the operator claims. The
// operator creates and owns this class (see GatewayClassReconciler); each of its
// Gateways becomes a NetBird reverse-proxy instance, configured by the
// ReverseProxyClusterParameters the Gateway points at via
// spec.infrastructure.parametersRef.
const gatewayController gwv1.GatewayController = "netbird.io/gateway-controller"

// paramsKind is the kind a Gateway's infrastructure.parametersRef must reference.
const paramsKind = "ReverseProxyClusterParameters"

// httpRouteLabel marks the ReverseProxyService children translated from an
// HTTPRoute (value = the route's safe-name, see httpRouteLabelValue), for prune
// and lookup.
const httpRouteLabel = "gateway.netbird.io/httproute"

// Kubernetes name/label-value length caps.
const (
	labelValueMaxLen = 63
	objectNameMaxLen = 253
)

// safeName truncates s to max bytes, replacing the tail with a short hash of the
// full value when it must cut. The result stays unique per distinct input and
// ends on an alphanumeric (valid for both DNS-1123 names and label values).
func safeName(s string, max int) string {
	if len(s) <= max {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	head := strings.TrimRight(s[:max-len(suffix)], "-._")
	return head + suffix
}

// httpRouteLabelValue is the label value stamped on a route's translated
// children. It must equal what prune lists on, and fit the 63-char label cap (an
// HTTPRoute name may be up to 253).
func httpRouteLabelValue(route *gwv1.HTTPRoute) string {
	return safeName(route.Name, labelValueMaxLen)
}

// gatewayProxy is the per-Gateway proxy config derived from its listeners.
type gatewayProxy struct {
	domain         string // listener hostname minus "*."  -> ccbash.io
	clusterAddress string // convention: "gate." + domain   -> gate.ccbash.io
	certSecret     string // listener tls.certificateRefs[0]
	listener       gwv1.SectionName
}

// gatewayProxyConfig derives the proxy config from the first TLS-terminating
// listener that has a hostname and a certificate. ok is false when no such
// listener exists (the Gateway can't front a proxy).
func gatewayProxyConfig(gw *gwv1.Gateway) (gatewayProxy, bool) {
	for _, l := range gw.Spec.Listeners {
		tlsListener := l.Protocol == gwv1.HTTPSProtocolType || l.Protocol == gwv1.TLSProtocolType
		if !tlsListener || l.Hostname == nil || *l.Hostname == "" || l.TLS == nil || len(l.TLS.CertificateRefs) == 0 {
			continue
		}
		domain := strings.TrimPrefix(string(*l.Hostname), "*.")
		return gatewayProxy{
			domain:         domain,
			clusterAddress: "gate." + domain,
			certSecret:     string(l.TLS.CertificateRefs[0].Name),
			listener:       l.Name,
		}, true
	}
	return gatewayProxy{}, false
}

func proxyClusterNameForGateway(gw *gwv1.Gateway) string {
	return "gateway-" + gw.Name
}

// resolveGatewayParams returns the ReverseProxyClusterParameters a Gateway
// references via spec.infrastructure.parametersRef (a namespace-local ref, so
// the parameters must live in the Gateway's namespace), or an error describing
// why it can't be resolved.
func resolveGatewayParams(ctx context.Context, c client.Client, gw *gwv1.Gateway) (*nbv1alpha1.ReverseProxyClusterParameters, error) {
	if gw.Spec.Infrastructure == nil || gw.Spec.Infrastructure.ParametersRef == nil {
		return nil, fmt.Errorf("spec.infrastructure.parametersRef is required and must reference a %s", paramsKind)
	}
	ref := gw.Spec.Infrastructure.ParametersRef
	if string(ref.Group) != nbv1alpha1.GroupVersion.Group || string(ref.Kind) != paramsKind {
		return nil, fmt.Errorf("parametersRef must reference %s.%s, got %s.%s", paramsKind, nbv1alpha1.GroupVersion.Group, ref.Kind, ref.Group)
	}
	params := &nbv1alpha1.ReverseProxyClusterParameters{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: gw.Namespace, Name: ref.Name}, params); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, fmt.Errorf("%s %q not found in namespace %q", paramsKind, ref.Name, gw.Namespace)
		}
		return nil, err
	}
	return params, nil
}

// GatewayClassReconciler owns the operator's GatewayClass: it ensures the
// managed class (ManagedClassName, controllerName gatewayController) exists,
// recreates it if deleted, and marks any class of our controllerName Accepted.
type GatewayClassReconciler struct {
	client.Client
	// ManagedClassName is the GatewayClass the operator creates and self-heals.
	ManagedClassName string
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/status,verbs=get;update;patch

func (r *GatewayClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	gc := &gwv1.GatewayClass{}
	if err := r.Get(ctx, req.NamespacedName, gc); err != nil {
		if kerrors.IsNotFound(err) && req.Name == r.ManagedClassName {
			// Our managed class was deleted out of band — recreate it.
			return ctrl.Result{}, r.ensure(ctx)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if gc.Spec.ControllerName != gatewayController {
		return ctrl.Result{}, nil
	}

	meta.SetStatusCondition(&gc.Status.Conditions, metav1.Condition{
		Type:               string(gwv1.GatewayClassConditionStatusAccepted),
		Status:             metav1.ConditionTrue,
		Reason:             string(gwv1.GatewayClassReasonAccepted),
		Message:            "Accepted by the NetBird reverse-proxy gateway controller",
		ObservedGeneration: gc.Generation,
	})
	return ctrl.Result{}, r.Status().Update(ctx, gc)
}

// ensure server-side-applies the managed GatewayClass. It is idempotent and
// safe to call at startup and on every recreate. A pre-existing class with a
// different (immutable) controllerName makes the apply fail — surfaced to the
// caller, which logs rather than crashes.
func (r *GatewayClassReconciler) ensure(ctx context.Context) error {
	if r.ManagedClassName == "" {
		return nil
	}
	gcAC := gwv1ac.GatewayClass(r.ManagedClassName).
		WithSpec(gwv1ac.GatewayClassSpec().
			WithControllerName(gatewayController).
			WithDescription("NetBird reverse-proxy gateway (operator-managed)"))
	return r.Apply(ctx, gcAC, client.ForceOwnership)
}

func (r *GatewayClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gwv1.GatewayClass{}).
		WithLogConstructor(logConstructor(mgr, "GatewayClass")).
		Complete(r)
}

// Start ensures the managed GatewayClass exists once the manager is running. It
// is a leader-elected manager.Runnable (a plain func is treated as one), so only
// the active operator creates it; a failed apply (e.g. a stale class with the
// old controllerName) is logged, not fatal — the user deletes the stale class
// and the For-watch recreate path takes over.
func (r *GatewayClassReconciler) Start(ctx context.Context) error {
	if err := r.ensure(ctx); err != nil {
		logf.FromContext(ctx).Error(err, "could not ensure managed GatewayClass", "name", r.ManagedClassName)
	}
	return nil
}

// GatewayReconciler turns a Gateway of a BYOP class into a ReverseProxyCluster
// it owns (deriving domain/clusterAddress/cert from the Gateway's listeners and
// the rest from the class's parameters), and reflects the cluster's state back
// into the Gateway's status.
type GatewayReconciler struct {
	client.Client
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=netbird.io,resources=reverseproxyclusterparameters,verbs=get;list;watch

func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	gw := &gwv1.Gateway{}
	if err := r.Get(ctx, req.NamespacedName, gw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	gc := &gwv1.GatewayClass{}
	if err := r.Get(ctx, client.ObjectKey{Name: string(gw.Spec.GatewayClassName)}, gc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if gc.Spec.ControllerName != gatewayController {
		return ctrl.Result{}, nil // not ours
	}

	params, err := resolveGatewayParams(ctx, r.Client, gw)
	if err != nil {
		return r.fail(ctx, gw, "InvalidParameters", err.Error())
	}
	cfg, ok := gatewayProxyConfig(gw)
	if !ok {
		return r.fail(ctx, gw, "NoUsableListener", "no HTTPS/TLS listener with a hostname and certificateRefs")
	}

	ownerRef, err := k8sutil.ControllerReference(gw, r.Scheme())
	if err != nil {
		return ctrl.Result{}, err
	}

	spec := nbv1alpha1ac.ReverseProxyClusterSpec().
		WithClusterAddress(cfg.clusterAddress).
		WithDomain(cfg.domain).
		WithPrivate(params.Spec.Private)
	if cfg.certSecret != "" {
		spec.WithCertSecretName(cfg.certSecret)
	}
	if params.Spec.Image != "" {
		spec.WithImage(params.Spec.Image)
	}
	if params.Spec.Replicas != nil {
		spec.WithReplicas(*params.Spec.Replicas)
	}
	if len(params.Spec.Groups) > 0 {
		spec.WithGroups(groupRefACs(params.Spec.Groups)...)
	}
	if len(params.Spec.ServiceAnnotations) > 0 {
		spec.WithServiceAnnotations(params.Spec.ServiceAnnotations)
	}
	rpcAC := nbv1alpha1ac.ReverseProxyCluster(proxyClusterNameForGateway(gw), gw.Namespace).
		WithOwnerReferences(ownerRef).
		WithSpec(spec)
	if err := r.Apply(ctx, rpcAC, client.ForceOwnership); err != nil {
		return ctrl.Result{}, err
	}

	// Reflect the owned cluster's state into the Gateway status.
	rpc := &nbv1alpha1.ReverseProxyCluster{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: gw.Namespace, Name: proxyClusterNameForGateway(gw)}, rpc); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, r.syncStatus(ctx, gw, cfg, rpc)
}

// fail records an Accepted=False condition and requeues.
func (r *GatewayReconciler) fail(ctx context.Context, gw *gwv1.Gateway, reason, msg string) (ctrl.Result, error) {
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               string(gwv1.GatewayConditionAccepted),
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: gw.Generation,
	})
	if err := r.Status().Update(ctx, gw); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: dependencyRetry}, nil
}

// syncStatus sets the Gateway's Accepted/Programmed conditions, its address (the
// proxy LoadBalancer IP), and the chosen listener's status.
func (r *GatewayReconciler) syncStatus(ctx context.Context, gw *gwv1.Gateway, cfg gatewayProxy, rpc *nbv1alpha1.ReverseProxyCluster) error {
	meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
		Type:               string(gwv1.GatewayConditionAccepted),
		Status:             metav1.ConditionTrue,
		Reason:             string(gwv1.GatewayReasonAccepted),
		Message:            "Translated to a NetBird ReverseProxyCluster",
		ObservedGeneration: gw.Generation,
	})

	ready := meta.IsStatusConditionTrue(rpc.Status.Conditions, nbv1alpha1.ReadyCondition)
	programmed := metav1.Condition{
		Type:               string(gwv1.GatewayConditionProgrammed),
		Status:             metav1.ConditionFalse,
		Reason:             "Pending",
		Message:            "waiting for the proxy cluster to become ready",
		ObservedGeneration: gw.Generation,
	}
	if ready {
		programmed.Status = metav1.ConditionTrue
		programmed.Reason = string(gwv1.GatewayReasonProgrammed)
		programmed.Message = "the proxy cluster is ready"
	}
	meta.SetStatusCondition(&gw.Status.Conditions, programmed)

	if rpc.Status.LoadBalancerIP != "" {
		ipType := gwv1.IPAddressType
		gw.Status.Addresses = []gwv1.GatewayStatusAddress{{Type: &ipType, Value: rpc.Status.LoadBalancerIP}}
	}

	gw.Status.Listeners = r.listenerStatuses(ctx, gw, cfg, ready)
	return r.Status().Update(ctx, gw)
}

// listenerStatuses builds per-listener status: the chosen TLS listener gets full
// Accepted/ResolvedRefs/Programmed conditions; any other listener is marked
// unused (this implementation fronts a single TLS listener).
func (r *GatewayReconciler) listenerStatuses(ctx context.Context, gw *gwv1.Gateway, cfg gatewayProxy, programmed bool) []gwv1.ListenerStatus {
	attached := r.attachedRoutes(ctx, gw)
	httpRouteKind := gwv1.RouteGroupKind{Group: (*gwv1.Group)(&gwv1.GroupVersion.Group), Kind: "HTTPRoute"}

	out := make([]gwv1.ListenerStatus, 0, len(gw.Spec.Listeners))
	for _, l := range gw.Spec.Listeners {
		st := gwv1.ListenerStatus{
			Name:           l.Name,
			SupportedKinds: []gwv1.RouteGroupKind{httpRouteKind},
		}
		if l.Name != cfg.listener {
			meta.SetStatusCondition(&st.Conditions, metav1.Condition{
				Type: string(gwv1.ListenerConditionAccepted), Status: metav1.ConditionFalse,
				Reason: "Unused", Message: "this implementation fronts a single TLS listener", ObservedGeneration: gw.Generation,
			})
			out = append(out, st)
			continue
		}
		st.AttachedRoutes = attached
		certOK := r.secretExists(ctx, gw.Namespace, cfg.certSecret)
		resolved := metav1.Condition{
			Type: string(gwv1.ListenerConditionResolvedRefs), Status: metav1.ConditionTrue,
			Reason: string(gwv1.ListenerReasonResolvedRefs), Message: "certificate resolved", ObservedGeneration: gw.Generation,
		}
		if !certOK {
			resolved.Status = metav1.ConditionFalse
			resolved.Reason = string(gwv1.ListenerReasonInvalidCertificateRef)
			resolved.Message = fmt.Sprintf("Secret %q not found", cfg.certSecret)
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: string(gwv1.ListenerConditionAccepted), Status: metav1.ConditionTrue,
			Reason: string(gwv1.ListenerReasonAccepted), Message: "Accepted", ObservedGeneration: gw.Generation,
		})
		meta.SetStatusCondition(&st.Conditions, resolved)
		progStatus := metav1.ConditionFalse
		if programmed && certOK {
			progStatus = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: string(gwv1.ListenerConditionProgrammed), Status: progStatus,
			Reason: string(gwv1.ListenerReasonProgrammed), Message: "", ObservedGeneration: gw.Generation,
		})
		out = append(out, st)
	}
	return out
}

func (r *GatewayReconciler) secretExists(ctx context.Context, namespace, name string) bool {
	if name == "" {
		return false
	}
	return r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &corev1.Secret{}) == nil
}

// attachedRoutes counts HTTPRoutes that reference this Gateway as a parent.
func (r *GatewayReconciler) attachedRoutes(ctx context.Context, gw *gwv1.Gateway) int32 {
	var routes gwv1.HTTPRouteList
	if err := r.List(ctx, &routes); err != nil {
		return 0
	}
	var n int32
	for i := range routes.Items {
		if routeReferencesGateway(&routes.Items[i], gw) {
			n++
		}
	}
	return n
}

func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gwv1.Gateway{}).
		WithLogConstructor(logConstructor(mgr, "Gateway")).
		Owns(&nbv1alpha1.ReverseProxyCluster{}).
		Watches(&gwv1.GatewayClass{}, handler.EnqueueRequestsFromMapFunc(r.gatewaysForClass)).
		Watches(&nbv1alpha1.ReverseProxyClusterParameters{}, handler.EnqueueRequestsFromMapFunc(r.gatewaysForParams)).
		Complete(r)
}

// gatewaysForClass enqueues Gateways whose gatewayClassName is the changed class.
func (r *GatewayReconciler) gatewaysForClass(ctx context.Context, obj client.Object) []reconcile.Request {
	var list gwv1.GatewayList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if string(list.Items[i].Spec.GatewayClassName) == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

// gatewaysForParams enqueues Gateways in the changed parameters' namespace whose
// infrastructure.parametersRef names it (the reconcile filters to our class).
func (r *GatewayReconciler) gatewaysForParams(ctx context.Context, obj client.Object) []reconcile.Request {
	var list gwv1.GatewayList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		infra := list.Items[i].Spec.Infrastructure
		if infra != nil && infra.ParametersRef != nil && infra.ParametersRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

// HTTPRouteReconciler translates HTTPRoutes attached to a BYOP Gateway into
// owned ReverseProxyService children — the same exposure the operator already
// reconciles, authored from Gateway API.
type HTTPRouteReconciler struct {
	client.Client
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes/status,verbs=get;update;patch

func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	route := &gwv1.HTTPRoute{}
	if err := r.Get(ctx, req.NamespacedName, route); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	gw, parent, ok, err := r.resolveGateway(ctx, route)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ok {
		// Not attached to one of our Gateways — drop anything we previously made.
		return ctrl.Result{}, r.prune(ctx, route, nil)
	}
	cfg, ok := gatewayProxyConfig(gw)
	if !ok {
		return ctrl.Result{RequeueAfter: dependencyRetry}, r.prune(ctx, route, nil)
	}

	ownerRef, err := k8sutil.ControllerReference(route, r.Scheme())
	if err != nil {
		return ctrl.Result{}, err
	}

	// Fail closed: if a rule uses a path match we can't faithfully represent
	// (Exact/RegularExpression), reject the whole route rather than silently
	// widening it to "/" and over-exposing the backend.
	backends, ok := routeBackends(route)
	if !ok {
		if err := r.prune(ctx, route, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.setRouteCondition(ctx, route, parent, metav1.ConditionFalse,
			gwv1.RouteReasonUnsupportedValue, "only PathPrefix path matches are supported")
	}

	desired := map[string]bool{}
	for _, hostname := range route.Spec.Hostnames {
		if !gatewayAdmitsRoute(gw, route, string(hostname), cfg.listener) {
			continue // hostname doesn't match the proxy listener / namespace not allowed
		}
		name := routeChildName(route.Name, string(hostname))
		desired[name] = true
		rpsAC := nbv1alpha1ac.ReverseProxyService(name, route.Namespace).
			WithOwnerReferences(ownerRef).
			WithLabels(map[string]string{httpRouteLabel: httpRouteLabelValue(route)}).
			WithSpec(nbv1alpha1ac.ReverseProxyServiceSpec().
				WithDomain(string(hostname)).
				WithProxyCluster(cfg.clusterAddress).
				// Pass the original Host to the backend, as a Gateway/kgateway
				// would — apps front themselves by their public hostname.
				WithPassHostHeader(true).
				WithBackends(backends...))
		if err := r.Apply(ctx, rpsAC, client.ForceOwnership); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := r.prune(ctx, route, desired); err != nil {
		return ctrl.Result{}, err
	}

	status, reason, msg := metav1.ConditionTrue, gwv1.RouteReasonAccepted, "Translated to a NetBird ReverseProxyService"
	if len(desired) == 0 {
		status, reason, msg = metav1.ConditionFalse, gwv1.RouteReasonNoMatchingListenerHostname, "no route hostname matches the Gateway's proxy listener"
	}
	if err := r.setRouteCondition(ctx, route, parent, status, reason, msg); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// resolveGateway finds the first parent Gateway of a BYOP class the operator
// owns, returning it plus the matched parentRef.
func (r *HTTPRouteReconciler) resolveGateway(ctx context.Context, route *gwv1.HTTPRoute) (*gwv1.Gateway, gwv1.ParentReference, bool, error) {
	for _, p := range route.Spec.ParentRefs {
		ns := route.Namespace
		if p.Namespace != nil {
			ns = string(*p.Namespace)
		}
		gw := &gwv1.Gateway{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: string(p.Name)}, gw); err != nil {
			if kerrors.IsNotFound(err) {
				continue
			}
			return nil, p, false, err
		}
		gc := &gwv1.GatewayClass{}
		if err := r.Get(ctx, client.ObjectKey{Name: string(gw.Spec.GatewayClassName)}, gc); err != nil {
			if kerrors.IsNotFound(err) {
				continue
			}
			return nil, p, false, err
		}
		if gc.Spec.ControllerName != gatewayController {
			continue
		}
		return gw, p, true, nil
	}
	return nil, gwv1.ParentReference{}, false, nil
}

// prune deletes ReverseProxyService children of the route no longer desired
// (desired nil drops them all).
func (r *HTTPRouteReconciler) prune(ctx context.Context, route *gwv1.HTTPRoute, desired map[string]bool) error {
	var list nbv1alpha1.ReverseProxyServiceList
	if err := r.List(ctx, &list, client.InNamespace(route.Namespace), client.MatchingLabels{httpRouteLabel: httpRouteLabelValue(route)}); err != nil {
		return err
	}
	for i := range list.Items {
		if !desired[list.Items[i].Name] {
			if err := r.Delete(ctx, &list.Items[i]); err != nil && !kerrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

// setRouteCondition sets the Accepted condition on the route's parent status
// entry for our controller, with the given status/reason/message.
func (r *HTTPRouteReconciler) setRouteCondition(ctx context.Context, route *gwv1.HTTPRoute, parent gwv1.ParentReference, status metav1.ConditionStatus, reason gwv1.RouteConditionReason, message string) error {
	cond := metav1.Condition{
		Type:               string(gwv1.RouteConditionAccepted),
		Status:             status,
		Reason:             string(reason),
		Message:            message,
		ObservedGeneration: route.Generation,
	}
	idx := -1
	for i := range route.Status.Parents {
		if route.Status.Parents[i].ControllerName == gatewayController {
			idx = i
			break
		}
	}
	if idx < 0 {
		route.Status.Parents = append(route.Status.Parents, gwv1.RouteParentStatus{
			ParentRef:      parent,
			ControllerName: gatewayController,
		})
		idx = len(route.Status.Parents) - 1
	}
	meta.SetStatusCondition(&route.Status.Parents[idx].Conditions, cond)
	return r.Status().Update(ctx, route)
}

func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gwv1.HTTPRoute{}).
		WithLogConstructor(logConstructor(mgr, "HTTPRoute")).
		Owns(&nbv1alpha1.ReverseProxyService{}).
		Watches(&gwv1.Gateway{}, handler.EnqueueRequestsFromMapFunc(r.routesForGateway)).
		Complete(r)
}

// routesForGateway enqueues HTTPRoutes that reference the changed Gateway.
func (r *HTTPRouteReconciler) routesForGateway(ctx context.Context, obj client.Object) []reconcile.Request {
	gw, ok := obj.(*gwv1.Gateway)
	if !ok {
		return nil
	}
	var routes gwv1.HTTPRouteList
	if err := r.List(ctx, &routes); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range routes.Items {
		if routeReferencesGateway(&routes.Items[i], gw) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&routes.Items[i])})
		}
	}
	return reqs
}

// routeReferencesGateway reports whether the route lists gw as a parentRef.
func routeReferencesGateway(route *gwv1.HTTPRoute, gw *gwv1.Gateway) bool {
	for _, p := range route.Spec.ParentRefs {
		ns := route.Namespace
		if p.Namespace != nil {
			ns = string(*p.Namespace)
		}
		if string(p.Name) == gw.Name && ns == gw.Namespace {
			return true
		}
	}
	return false
}

// gatewayAdmitsRoute reports whether the route (for the given hostname) attaches
// to the Gateway's proxy listener: it must match *that* listener (the one that
// defines the cluster's domain/clusterAddress, not just any listener) and its
// allowedRoutes must permit the route's namespace. Restricting to the proxy
// listener avoids admitting a route under a second listener's domain and then
// registering it against the first listener's (wrong) proxy cluster.
func gatewayAdmitsRoute(gw *gwv1.Gateway, route *gwv1.HTTPRoute, hostname string, proxyListener gwv1.SectionName) bool {
	for _, l := range gw.Spec.Listeners {
		if l.Name != proxyListener {
			continue
		}
		return listenerHostnameMatches(l.Hostname, hostname) && allowedRoutesPermits(gw, l, route.Namespace)
	}
	return false
}

// listenerHostnameMatches matches a route hostname against a listener hostname,
// honoring a leading wildcard label. A nil/empty listener hostname matches all.
func listenerHostnameMatches(listener *gwv1.Hostname, routeHost string) bool {
	if listener == nil || *listener == "" {
		return true
	}
	lh := string(*listener)
	if suffix, ok := strings.CutPrefix(lh, "*."); ok {
		return strings.HasSuffix(routeHost, "."+suffix) && len(routeHost) > len(suffix)+1
	}
	return lh == routeHost
}

// allowedRoutesPermits checks the listener's allowedRoutes namespace policy
// (From All or Same; Selector is treated as deny in this implementation).
func allowedRoutesPermits(gw *gwv1.Gateway, l gwv1.Listener, routeNamespace string) bool {
	if l.AllowedRoutes == nil || l.AllowedRoutes.Namespaces == nil || l.AllowedRoutes.Namespaces.From == nil {
		return routeNamespace == gw.Namespace // default: Same
	}
	switch *l.AllowedRoutes.Namespaces.From {
	case gwv1.NamespacesFromAll:
		return true
	case gwv1.NamespacesFromSame:
		return routeNamespace == gw.Namespace
	default:
		return false
	}
}

// routeBackends maps an HTTPRoute's rules onto ReverseProxyService backends: one
// backend per (rule, backendRef), carrying the rule's PathPrefix as the path. ok
// is false when a rule uses a path match type we can't represent (Exact /
// RegularExpression) — the caller fails the route closed rather than widening it.
// (Header/method matches are not translated; only the path prefix is.)
func routeBackends(route *gwv1.HTTPRoute) ([]*nbv1alpha1ac.ReverseProxyBackendApplyConfiguration, bool) {
	var out []*nbv1alpha1ac.ReverseProxyBackendApplyConfiguration
	for _, rule := range route.Spec.Rules {
		path, ok := rulePath(rule)
		if !ok {
			return nil, false
		}
		for _, br := range rule.BackendRefs {
			b := nbv1alpha1ac.ReverseProxyBackend().
				WithServiceRef(corev1.LocalObjectReference{Name: string(br.Name)}).
				WithPath(path)
			if br.Port != nil {
				b.WithPort(int(*br.Port))
			}
			out = append(out, b)
		}
	}
	return out, true
}

// rulePath returns the rule's PathPrefix match value (or "/" when the rule has no
// path match). ok is false for an Exact/RegularExpression path match, which the
// NetBird proxy's prefix-only model can't faithfully represent.
func rulePath(rule gwv1.HTTPRouteRule) (string, bool) {
	for _, m := range rule.Matches {
		if m.Path == nil || m.Path.Type == nil {
			continue
		}
		if *m.Path.Type != gwv1.PathMatchPathPrefix {
			return "", false
		}
		if m.Path.Value != nil {
			return *m.Path.Value, true
		}
	}
	return "/", true
}

// routeChildName derives a DNS-safe ReverseProxyService name from the route name
// and a hostname (which carries dots and may be a wildcard). Over-long names are
// hash-truncated (safeName) so distinct routes/hostnames don't collide and the
// result never ends on an invalid character.
func routeChildName(routeName, hostname string) string {
	h := strings.ReplaceAll(hostname, "*", "wildcard")
	h = strings.ReplaceAll(h, ".", "-")
	return safeName(routeName+"-"+h, objectNameMaxLen)
}
