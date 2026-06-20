// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/patch"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/k8sutil"
	"github.com/netbirdio/kubernetes-operator/internal/netbirdutil"
	nbv1alpha1ac "github.com/netbirdio/kubernetes-operator/pkg/applyconfigurations/api/v1alpha1"
)

// NetworkRouterReconciler creates a NetBird router (a peer group bound to a
// network) and, for peers.deploy, the netbird-client DaemonSet behind it.
type NetworkRouterReconciler struct {
	client.Client

	Netbird       *netbird.Client
	ClientImage   string
	ManagementURL string
	Recorder      record.EventRecorder
}

// +kubebuilder:rbac:groups=netbird.io,resources=networkrouters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=netbird.io,resources=networkrouters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=netbird.io,resources=networkrouters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete

func (r *NetworkRouterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	nr := &nbv1alpha1.NetworkRouter{}
	if err := r.Get(ctx, req.NamespacedName, nr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	sp := patch.NewSerialPatcher(nr, r.Client)
	logf.FromContext(ctx).V(1).Info("reconciling NetworkRouter")

	if !nr.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, sp, nr)
	}

	controllerutil.AddFinalizer(nr, k8sutil.Finalizer("networkrouter"))
	if err := sp.Patch(ctx, nr); err != nil {
		return ctrl.Result{}, err
	}

	networkID, err := resolveNetworkID(ctx, r.Client, nr.Spec.NetworkRef)
	if err != nil {
		return r.dependency(ctx, sp, nr, err)
	}
	nr.Status.NetworkID = networkID

	groupID, err := r.resolvePeerGroup(ctx, nr)
	if err != nil {
		return r.dependency(ctx, sp, nr, err)
	}
	nr.Status.GroupID = groupID

	routerID, err := r.upsertRouter(ctx, networkID, nr.Status.RouterID, groupID, nr.Spec)
	if err != nil {
		return ctrl.Result{}, err
	}
	nr.Status.RouterID = routerID

	conditions.MarkTrue(nr, nbv1alpha1.ReadyCondition, nbv1alpha1.ReconciledReason, "")
	if err := sp.Patch(ctx, nr, patch.WithStatusObservedGeneration{}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// dependency marks the router not-ready and requeues for errDependencyNotReady,
// otherwise returns the error.
func (r *NetworkRouterReconciler) dependency(ctx context.Context, sp *patch.SerialPatcher, nr *nbv1alpha1.NetworkRouter, err error) (ctrl.Result, error) {
	if !errors.Is(err, errDependencyNotReady) {
		return ctrl.Result{}, err
	}
	conditions.MarkFalse(nr, nbv1alpha1.ReadyCondition, nbv1alpha1.DependencyReason, "%s", err.Error())
	if perr := sp.Patch(ctx, nr); perr != nil {
		return ctrl.Result{}, perr
	}
	return ctrl.Result{RequeueAfter: dependencyRetry}, nil
}

// resolvePeerGroup returns the NetBird group id of the routing peers. For
// peers.group it resolves the referenced group; for peers.deploy it ensures the
// Group, SetupKey and DaemonSet exist and are ready, returning
// errDependencyNotReady while they come up.
func (r *NetworkRouterReconciler) resolvePeerGroup(ctx context.Context, nr *nbv1alpha1.NetworkRouter) (string, error) {
	if g := nr.Spec.Peers.Group; g != nil {
		ids, err := netbirdutil.GetGroupIDs(ctx, r.Client, r.Netbird, []nbv1alpha1.GroupReference{*g}, nr.Namespace)
		if err != nil {
			return "", err
		}
		if len(ids) == 0 || ids[0] == "" {
			return "", fmt.Errorf("%w: peer group not resolved", errDependencyNotReady)
		}
		return ids[0], nil
	}
	return r.ensureDeployedPeers(ctx, nr)
}

func (r *NetworkRouterReconciler) ensureDeployedPeers(ctx context.Context, nr *nbv1alpha1.NetworkRouter) (string, error) {
	ownerRef, err := k8sutil.ControllerReference(nr, r.Scheme())
	if err != nil {
		return "", err
	}
	name := nr.Name + "-router"
	groupName := fmt.Sprintf("networkrouter-%s-%s", nr.Namespace, nr.Name)

	groupAC := nbv1alpha1ac.Group(name, nr.Namespace).
		WithOwnerReferences(ownerRef).
		WithSpec(nbv1alpha1ac.GroupSpec().WithName(groupName))
	if err := r.Apply(ctx, groupAC, client.ForceOwnership); err != nil {
		return "", err
	}
	group := &nbv1alpha1.Group{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: nr.Namespace, Name: name}, group); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if group.Status.GroupID == "" {
		return "", fmt.Errorf("%w: router group not ready", errDependencyNotReady)
	}

	setupKeyAC := nbv1alpha1ac.SetupKey(name, nr.Namespace).
		WithOwnerReferences(ownerRef).
		WithSpec(nbv1alpha1ac.SetupKeySpec().
			WithName(groupName).
			WithEphemeral(true).
			WithAutoGroups(nbv1alpha1ac.GroupReference().WithLocalRef(corev1.LocalObjectReference{Name: name})))
	if err := r.Apply(ctx, setupKeyAC, client.ForceOwnership); err != nil {
		return "", err
	}
	setupKey := &nbv1alpha1.SetupKey{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: nr.Namespace, Name: name}, setupKey); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if setupKey.Status.SetupKeyID == "" {
		return "", fmt.Errorf("%w: router setup key not ready", errDependencyNotReady)
	}

	if err := r.ensureDaemonSet(ctx, nr, ownerRef, setupKey); err != nil {
		return "", err
	}
	ds := &appsv1.DaemonSet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: nr.Namespace, Name: name}, ds); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if ds.Status.DesiredNumberScheduled == 0 || ds.Status.NumberReady != ds.Status.DesiredNumberScheduled {
		return "", fmt.Errorf("%w: router pods not ready", errDependencyNotReady)
	}
	return group.Status.GroupID, nil
}

func (r *NetworkRouterReconciler) upsertRouter(ctx context.Context, networkID, routerID, groupID string, spec nbv1alpha1.NetworkRouterSpec) (string, error) {
	routers := r.Netbird.Networks.Routers(networkID)
	peerGroups := []string{groupID}
	req := api.NetworkRouterRequest{
		Enabled:    spec.Enabled,
		Masquerade: spec.Masquerade,
		Metric:     spec.Metric,
		PeerGroups: &peerGroups,
	}
	if routerID != "" {
		resp, err := routers.Update(ctx, routerID, req)
		if err == nil {
			return resp.Id, nil
		}
		if !netbird.IsNotFound(err) {
			return "", err
		}
	}
	resp, err := routers.Create(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Id, nil
}

func (r *NetworkRouterReconciler) reconcileDelete(ctx context.Context, sp *patch.SerialPatcher, nr *nbv1alpha1.NetworkRouter) (ctrl.Result, error) {
	if nr.Status.NetworkID != "" && nr.Status.RouterID != "" {
		err := r.Netbird.Networks.Routers(nr.Status.NetworkID).Delete(ctx, nr.Status.RouterID)
		if err != nil && !netbird.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	controllerutil.RemoveFinalizer(nr, k8sutil.Finalizer("networkrouter"))
	if err := sp.Patch(ctx, nr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *NetworkRouterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nbv1alpha1.NetworkRouter{}).
		WithLogConstructor(logConstructor(mgr, "NetworkRouter")).
		Owns(&nbv1alpha1.Group{}).
		Owns(&nbv1alpha1.SetupKey{}).
		Owns(&appsv1.DaemonSet{}).
		Complete(r)
}

// ensureDaemonSet applies the hostNetwork netbird-client DaemonSet that turns
// each selected node into a routing peer.
func (r *NetworkRouterReconciler) ensureDaemonSet(ctx context.Context, nr *nbv1alpha1.NetworkRouter, ownerRef *metav1ac.OwnerReferenceApplyConfiguration, setupKey *nbv1alpha1.SetupKey) error {
	name := nr.Name + "-router"
	selectorLabels := map[string]string{
		"app.kubernetes.io/name":     "netbird-router",
		"app.kubernetes.io/instance": nr.Name,
	}

	deploy := nr.Spec.Peers.Deploy
	image := r.ClientImage
	logLevel := "info"
	if deploy != nil {
		if deploy.Image != "" {
			image = deploy.Image
		}
		if deploy.LogLevel != "" {
			logLevel = deploy.LogLevel
		}
	}

	podSpec := corev1ac.PodSpec().
		WithHostNetwork(true).
		WithDNSPolicy(corev1.DNSClusterFirstWithHostNet).
		WithContainers(corev1ac.Container().
			WithName("netbird").
			WithImage(image).
			WithEnv(
				corev1ac.EnvVar().WithName("NB_SETUP_KEY").WithValueFrom(corev1ac.EnvVarSource().
					WithSecretKeyRef(corev1ac.SecretKeySelector().WithName(setupKey.SecretName()).WithKey(SetupKeySecretKey))),
				corev1ac.EnvVar().WithName("NB_MANAGEMENT_URL").WithValue(r.ManagementURL),
				corev1ac.EnvVar().WithName("NB_LOG_LEVEL").WithValue(logLevel),
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
			).
			WithSecurityContext(corev1ac.SecurityContext().
				WithCapabilities(corev1ac.Capabilities().WithAdd("NET_ADMIN").WithAdd("SYS_RESOURCE").WithAdd("SYS_ADMIN")).
				WithPrivileged(true)).
			WithResources(corev1ac.ResourceRequirements().WithRequests(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			}))).
		WithVolumes(
			corev1ac.Volume().WithName("netbird-run").WithEmptyDir(corev1ac.EmptyDirVolumeSource()),
			corev1ac.Volume().WithName("netbird-lib").WithEmptyDir(corev1ac.EmptyDirVolumeSource()),
		)
	if deploy != nil && len(deploy.NodeSelector) > 0 {
		podSpec = podSpec.WithNodeSelector(deploy.NodeSelector)
	}

	dsAC := appsv1ac.DaemonSet(name, nr.Namespace).
		WithOwnerReferences(ownerRef).
		WithLabels(selectorLabels).
		WithSpec(appsv1ac.DaemonSetSpec().
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(selectorLabels)).
			WithTemplate(corev1ac.PodTemplateSpec().WithLabels(selectorLabels).WithSpec(podSpec)))
	return r.Apply(ctx, dsAC, client.ForceOwnership)
}
