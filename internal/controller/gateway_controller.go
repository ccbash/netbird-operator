// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	policyv1ac "k8s.io/client-go/applyconfigurations/policy/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/gatewayutil"
	"github.com/netbirdio/kubernetes-operator/internal/k8sutil"
	nbv1alpha1ac "github.com/netbirdio/kubernetes-operator/pkg/applyconfigurations/api/v1alpha1"
)

// GatewayReconciler orchestrates the NetBird side of a netbird-class Gateway: it
// deploys the routing-peer pods and joins them to the referenced Network, owns
// the DNSZone derived from the listener hostname, and the Group/SetupKey the
// router peers use. Router-pod config uses operator defaults (image from
// --netbird-client-image, 3 replicas, info log level).
type GatewayReconciler struct {
	client.Client

	Netbird       *netbird.Client
	ManagementURL string
	ClientImage   string
	Recorder      record.EventRecorder
}

// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

func gatewayRouterName(gw *gwv1.Gateway) string { return gw.Name + "-router" }

// gatewayGroupName is the NetBird group name for a Gateway's router peers,
// qualified by namespace so two same-named Gateways don't collide account-wide.
func gatewayGroupName(gw *gwv1.Gateway) string {
	return fmt.Sprintf("gateway-%s-%s-router", gw.Namespace, gw.Name)
}

func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	gw := &gwv1.Gateway{}
	if err := r.Get(ctx, req.NamespacedName, gw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Only handle Gateways of the netbird GatewayClass.
	gwc := &gwv1.GatewayClass{}
	if err := r.Get(ctx, client.ObjectKey{Name: string(gw.Spec.GatewayClassName)}, gwc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if string(gwc.Spec.ControllerName) != GatewayControllerName {
		return ctrl.Result{}, nil
	}

	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("reconciling Gateway")

	if !gw.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, gw)
	}

	controllerutil.AddFinalizer(gw, k8sutil.Finalizer("gateway"))
	if err := r.Update(ctx, gw); err != nil {
		return ctrl.Result{}, err
	}

	networkName, err := gatewayutil.GetNetworkName(gw.Spec.Listeners)
	if err != nil {
		return r.notProgrammed(ctx, gw, "InvalidListener", err.Error())
	}
	hostname := ""
	if h := gw.Spec.Listeners[0].Hostname; h != nil {
		hostname = string(*h)
	}
	if hostname == "" {
		return r.notProgrammed(ctx, gw, "MissingHostname", "listener must set a hostname (the DNS zone domain)")
	}

	ownerRef, err := k8sutil.ControllerReference(gw, r.Scheme())
	if err != nil {
		return ctrl.Result{}, err
	}

	// The Network is admin-authored and referenced by the listener.
	network := &nbv1alpha1.Network{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: gw.Namespace, Name: networkName}, network); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return r.notProgrammed(ctx, gw, "NetworkNotFound", fmt.Sprintf("Network %q not found", networkName))
		}
		return ctrl.Result{}, err
	}
	if network.Status.NetworkID == "" {
		return r.notProgrammed(ctx, gw, "NetworkNotReady", "Network has no network id yet")
	}

	routerName := gatewayRouterName(gw)
	groupName := gatewayGroupName(gw)

	// Router peer group.
	groupAC := nbv1alpha1ac.Group(routerName, gw.Namespace).
		WithOwnerReferences(ownerRef).
		WithSpec(nbv1alpha1ac.GroupSpec().WithName(groupName))
	if err := r.Apply(ctx, groupAC, client.ForceOwnership); err != nil {
		return ctrl.Result{}, err
	}
	group := &nbv1alpha1.Group{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: gw.Namespace, Name: routerName}, group); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// DNS zone for the listener hostname, distributed to the router peers.
	zoneAC := nbv1alpha1ac.DNSZone(gatewayDNSZoneName(gw), gw.Namespace).
		WithOwnerReferences(ownerRef).
		WithSpec(nbv1alpha1ac.DNSZoneSpec().
			WithName(hostname).
			WithDomain(hostname).
			WithEnabled(true).
			WithDistributionGroups(nbv1alpha1ac.GroupReference().
				WithLocalRef(corev1.LocalObjectReference{Name: routerName})),
		)
	if err := r.Apply(ctx, zoneAC, client.ForceOwnership); err != nil {
		return ctrl.Result{}, err
	}

	// Setup key the router pods use to join, auto-joining the router group.
	setupKeyAC := nbv1alpha1ac.SetupKey(routerName, gw.Namespace).
		WithOwnerReferences(ownerRef).
		WithSpec(nbv1alpha1ac.SetupKeySpec().
			WithName(groupName).
			WithEphemeral(true).
			WithAutoGroups(nbv1alpha1ac.GroupReference().WithLocalRef(corev1.LocalObjectReference{Name: routerName})),
		)
	if err := r.Apply(ctx, setupKeyAC, client.ForceOwnership); err != nil {
		return ctrl.Result{}, err
	}
	setupKey := &nbv1alpha1.SetupKey{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: gw.Namespace, Name: routerName}, setupKey); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if setupKey.Status.SetupKeyID == "" {
		return r.notProgrammed(ctx, gw, "SetupKeyNotReady", "router setup key not ready yet")
	}

	// Router-pod deployment.
	if err := r.ensureDeployment(ctx, gw, ownerRef, setupKey); err != nil {
		return ctrl.Result{}, err
	}
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: gw.Namespace, Name: routerName}, dep); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if dep.Status.ReadyReplicas == 0 || dep.Status.ReadyReplicas != dep.Status.Replicas {
		return r.notProgrammed(ctx, gw, "RouterNotReady", "router pods not ready yet")
	}

	// Join the router peers to the network.
	if group.Status.GroupID == "" {
		return r.notProgrammed(ctx, gw, "GroupNotReady", "router group not ready yet")
	}
	if err := r.ensureRouter(ctx, network.Status.NetworkID, group.Status.GroupID); err != nil {
		return ctrl.Result{}, err
	}

	return r.programmed(ctx, gw)
}

func (r *GatewayReconciler) ensureRouter(ctx context.Context, networkID, groupID string) error {
	routers := r.Netbird.Networks.Routers(networkID)
	list, err := routers.List(ctx)
	if err != nil {
		return err
	}
	peerGroups := []string{groupID}
	req := api.NetworkRouterRequest{
		Enabled:    true,
		Masquerade: true,
		Metric:     9999,
		PeerGroups: &peerGroups,
	}
	for i := range list {
		if list[i].PeerGroups != nil && slices.Contains(*list[i].PeerGroups, groupID) {
			_, err := routers.Update(ctx, list[i].Id, req)
			return err
		}
	}
	_, err = routers.Create(ctx, req)
	return err
}

func (r *GatewayReconciler) reconcileDelete(ctx context.Context, gw *gwv1.Gateway) (ctrl.Result, error) {
	// Delete the NetBird router (a non-CRD API object) while the owned Group CRD
	// still exists to identify it. The Group/SetupKey/DNSZone/Deployment are
	// garbage-collected once the finalizer is removed.
	networkName, err := gatewayutil.GetNetworkName(gw.Spec.Listeners)
	if err == nil {
		network := &nbv1alpha1.Network{}
		group := &nbv1alpha1.Group{}
		nErr := r.Get(ctx, client.ObjectKey{Namespace: gw.Namespace, Name: networkName}, network)
		gErr := r.Get(ctx, client.ObjectKey{Namespace: gw.Namespace, Name: gatewayRouterName(gw)}, group)
		if nErr == nil && gErr == nil && network.Status.NetworkID != "" && group.Status.GroupID != "" {
			if err := r.deleteRouter(ctx, network.Status.NetworkID, group.Status.GroupID); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	controllerutil.RemoveFinalizer(gw, k8sutil.Finalizer("gateway"))
	if err := r.Update(ctx, gw); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *GatewayReconciler) deleteRouter(ctx context.Context, networkID, groupID string) error {
	routers := r.Netbird.Networks.Routers(networkID)
	list, err := routers.List(ctx)
	if err != nil {
		if netbird.IsNotFound(err) {
			return nil
		}
		return err
	}
	for i := range list {
		if list[i].PeerGroups != nil && slices.Contains(*list[i].PeerGroups, groupID) {
			if err := routers.Delete(ctx, list[i].Id); err != nil && !netbird.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

// notProgrammed marks the Gateway not-yet-programmed and requeues.
func (r *GatewayReconciler) notProgrammed(ctx context.Context, gw *gwv1.Gateway, reason, msg string) (ctrl.Result, error) {
	recordEvent(r.Recorder, gw, reasonDependencyNotReady, "%s", msg)
	if err := r.setConditions(ctx, gw, metav1.ConditionFalse, reason, msg); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: gatewayPoll}, nil
}

func (r *GatewayReconciler) programmed(ctx context.Context, gw *gwv1.Gateway) (ctrl.Result, error) {
	if err := r.setConditions(ctx, gw, metav1.ConditionTrue, string(gwv1.GatewayReasonProgrammed), "router ready"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

func (r *GatewayReconciler) setConditions(ctx context.Context, gw *gwv1.Gateway, programmed metav1.ConditionStatus, reason, msg string) error {
	conds := []metav1.Condition{
		{
			Type:               string(gwv1.GatewayConditionAccepted),
			Status:             metav1.ConditionTrue,
			Reason:             string(gwv1.GatewayReasonAccepted),
			ObservedGeneration: gw.Generation,
		},
		{
			Type:               string(gwv1.GatewayConditionProgrammed),
			Status:             programmed,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: gw.Generation,
		},
	}
	for _, c := range conds {
		apimeta.SetStatusCondition(&gw.Status.Conditions, c)
	}
	return r.Status().Update(ctx, gw)
}

func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gwv1.Gateway{}).
		WithLogConstructor(logConstructor(mgr, "Gateway")).
		Owns(&nbv1alpha1.Group{}).
		Owns(&nbv1alpha1.SetupKey{}).
		Owns(&nbv1alpha1.DNSZone{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}

// ensureDeployment applies the router-pod Deployment and its PodDisruptionBudget
// using operator defaults.
func (r *GatewayReconciler) ensureDeployment(ctx context.Context, gw *gwv1.Gateway, ownerRef *metav1ac.OwnerReferenceApplyConfiguration, setupKey *nbv1alpha1.SetupKey) error {
	name := gatewayRouterName(gw)
	selectorLabels := map[string]string{
		"app.kubernetes.io/name":     "netbird-router",
		"app.kubernetes.io/instance": gw.Name,
	}
	const replicas int32 = 3

	podTemplate := corev1ac.PodTemplateSpec().
		WithLabels(selectorLabels).
		WithSpec(corev1ac.PodSpec().
			WithTopologySpreadConstraints(corev1ac.TopologySpreadConstraint().
				WithMaxSkew(1).
				WithTopologyKey(corev1.LabelHostname).
				WithWhenUnsatisfiable(corev1.ScheduleAnyway).
				WithLabelSelector(metav1ac.LabelSelector().WithMatchLabels(selectorLabels)),
			).
			WithInitContainers(corev1ac.Container().
				WithName("resolv-conf").
				WithImage(r.ClientImage).
				WithCommand("sh", "-c", "cp /etc/resolv.conf /tmp/resolv.conf && cp /etc/resolv.conf /tmp/resolv.conf.original.netbird").
				WithVolumeMounts(corev1ac.VolumeMount().WithName("resolv-conf").WithMountPath("/tmp")).
				WithSecurityContext(corev1ac.SecurityContext().
					WithCapabilities(corev1ac.Capabilities().WithDrop("ALL")).
					WithReadOnlyRootFilesystem(true)),
			).
			WithContainers(corev1ac.Container().
				WithName("netbird").
				WithImage(r.ClientImage).
				WithEnv(
					corev1ac.EnvVar().WithName("NB_SETUP_KEY").WithValueFrom(corev1ac.EnvVarSource().
						WithSecretKeyRef(corev1ac.SecretKeySelector().WithName(setupKey.SecretName()).WithKey(SetupKeySecretKey))),
					corev1ac.EnvVar().WithName("NB_MANAGEMENT_URL").WithValue(r.ManagementURL),
					corev1ac.EnvVar().WithName("NB_LOG_LEVEL").WithValue("info"),
					corev1ac.EnvVar().WithName("NB_LOG_FILE").WithValue("console"),
					corev1ac.EnvVar().WithName("NB_DISABLE_PROFILES").WithValue("true"),
					corev1ac.EnvVar().WithName("NB_DISABLE_UPDATE_SETTINGS").WithValue("true"),
					corev1ac.EnvVar().WithName("NB_DAEMON_ADDR").WithValue("unix:///var/run/netbird/netbird.sock"),
					corev1ac.EnvVar().WithName("NB_ENTRYPOINT_SERVICE_TIMEOUT").WithValue("0"),
				).
				WithStartupProbe(corev1ac.Probe().WithExec(corev1ac.ExecAction().WithCommand("netbird", "status", "--check", "startup"))).
				WithReadinessProbe(corev1ac.Probe().WithExec(corev1ac.ExecAction().WithCommand("netbird", "status", "--check", "ready"))).
				WithVolumeMounts(
					corev1ac.VolumeMount().WithName("netbird-run").WithMountPath("/var/run/netbird"),
					corev1ac.VolumeMount().WithName("netbird-lib").WithMountPath("/var/lib/netbird"),
					corev1ac.VolumeMount().WithName("ssh-etc").WithMountPath("/etc/ssh"),
					corev1ac.VolumeMount().WithName("resolv-conf").WithMountPath("/etc/resolv.conf").WithSubPath("resolv.conf"),
					corev1ac.VolumeMount().WithName("resolv-conf").WithMountPath("/etc/resolv.conf.original.netbird").WithSubPath("resolv.conf.original.netbird"),
				).
				WithSecurityContext(corev1ac.SecurityContext().
					WithReadOnlyRootFilesystem(true).
					WithCapabilities(corev1ac.Capabilities().WithAdd("NET_ADMIN").WithAdd("SYS_RESOURCE").WithAdd("SYS_ADMIN")).
					WithPrivileged(true)).
				WithResources(corev1ac.ResourceRequirements().WithRequests(corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				})),
			).
			WithVolumes(
				corev1ac.Volume().WithName("netbird-run").WithEmptyDir(corev1ac.EmptyDirVolumeSource()),
				corev1ac.Volume().WithName("netbird-lib").WithEmptyDir(corev1ac.EmptyDirVolumeSource()),
				corev1ac.Volume().WithName("ssh-etc").WithEmptyDir(corev1ac.EmptyDirVolumeSource()),
				corev1ac.Volume().WithName("resolv-conf").WithEmptyDir(corev1ac.EmptyDirVolumeSource()),
			),
		)

	depAC := appsv1ac.Deployment(name, gw.Namespace).
		WithOwnerReferences(ownerRef).
		WithLabels(selectorLabels).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(replicas).
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(selectorLabels)).
			WithTemplate(podTemplate))
	if err := r.Apply(ctx, depAC, client.ForceOwnership); err != nil {
		return err
	}

	pdbAC := policyv1ac.PodDisruptionBudget(name, gw.Namespace).
		WithOwnerReferences(ownerRef).
		WithLabels(selectorLabels).
		WithSpec(policyv1ac.PodDisruptionBudgetSpec().
			WithMaxUnavailable(intstr.FromInt(1)).
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(selectorLabels)))
	return r.Apply(ctx, pdbAC, client.ForceOwnership)
}
