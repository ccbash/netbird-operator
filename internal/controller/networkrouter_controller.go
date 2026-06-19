// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/patch"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	policyv1ac "k8s.io/client-go/applyconfigurations/policy/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/k8sutil"
	"github.com/netbirdio/kubernetes-operator/internal/netbirdutil"
	nbv1alpha1ac "github.com/netbirdio/kubernetes-operator/pkg/applyconfigurations/api/v1alpha1"
)

type NetworkRouterReconciler struct {
	client.Client

	Netbird       *netbird.Client
	ManagementURL string
	ClientImage   string
	Recorder      record.EventRecorder
}

// +kubebuilder:rbac:groups=netbird.io,resources=networkrouters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=netbird.io,resources=networkrouters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=netbird.io,resources=networkrouters/finalizers,verbs=update

func (r *NetworkRouterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	netRouter := &nbv1alpha1.NetworkRouter{}
	err := r.Get(ctx, req.NamespacedName, netRouter)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ctrl.LoggerFrom(ctx).V(1).Info("reconciling network router")
	sp := patch.NewSerialPatcher(netRouter, r.Client)

	if !netRouter.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, sp, netRouter)
	}

	ownerRef, err := k8sutil.ControllerReference(netRouter, r.Scheme())
	if err != nil {
		return ctrl.Result{}, err
	}

	// Ensure the DNS Zone exists.
	_, err = netbirdutil.GetDNSZoneByName(ctx, r.Netbird, netRouter.Spec.DNSZoneRef.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	controllerutil.AddFinalizer(netRouter, k8sutil.Finalizer("networkrouter"))

	networkID, err := r.upsertNetwork(ctx, netRouter)
	if err != nil {
		return ctrl.Result{}, err
	}
	netRouter.Status.NetworkID = networkID
	err = sp.Patch(ctx, netRouter)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Calculate unique suffix used for Netbird resources.
	sum := sha256.Sum256([]byte(netRouter.UID))
	uniqueSuffix := networkID + "-" + fmt.Sprintf("%x", sum[:4])[:8]

	// Create the group used by the router to discover peers.
	groupAC := nbv1alpha1ac.Group(fmt.Sprintf("networkrouter-%s", netRouter.Name), req.Namespace).
		WithOwnerReferences(ownerRef).
		WithSpec(
			nbv1alpha1ac.GroupSpec().
				WithName(fmt.Sprintf("networkrouter-%s", uniqueSuffix)),
		)
	err = r.Client.Apply(ctx, groupAC, client.ForceOwnership)
	if err != nil {
		return ctrl.Result{}, err
	}
	group := &nbv1alpha1.Group{
		ObjectMeta: metav1.ObjectMeta{
			Name:      *groupAC.Name,
			Namespace: *groupAC.Namespace,
		},
	}
	err = r.Client.Get(ctx, client.ObjectKeyFromObject(group), group)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if group.Status.GroupID == "" {
		return ctrl.Result{}, nil
	}

	// Create the setup key used by routing peers.
	setupKeyAC := nbv1alpha1ac.SetupKey(fmt.Sprintf("networkrouter-%s", netRouter.Name), req.Namespace).
		WithOwnerReferences(ownerRef).
		WithSpec(
			nbv1alpha1ac.SetupKeySpec().
				WithName(fmt.Sprintf("networkrouter-%s", uniqueSuffix)).
				WithEphemeral(true).
				WithAutoGroups(nbv1alpha1ac.GroupReference().WithID(group.Status.GroupID)),
		)
	err = r.Client.Apply(ctx, setupKeyAC, client.ForceOwnership)
	if err != nil {
		return ctrl.Result{}, err
	}
	setupKey := nbv1alpha1.SetupKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:      *setupKeyAC.Name,
			Namespace: *setupKeyAC.Namespace,
		},
	}
	err = r.Get(ctx, client.ObjectKeyFromObject(&setupKey), &setupKey)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if setupKey.Status.SetupKeyID == "" {
		return ctrl.Result{}, nil
	}

	// Create the routing peer in netbird.
	routingPeerID, err := r.upsertRoutingPeer(ctx, networkID, netRouter.Status.RoutingPeerID, group.Status.GroupID)
	if err != nil {
		return ctrl.Result{}, err
	}
	netRouter.Status.RoutingPeerID = routingPeerID
	err = sp.Patch(ctx, netRouter, patch.WithStatusObservedGeneration{})
	if err != nil {
		return ctrl.Result{}, err
	}

	// Route the configured Service CIDRs into the network as subnet resources.
	if err := r.reconcileServiceCIDRs(ctx, sp, netRouter, networkID); err != nil {
		return ctrl.Result{}, err
	}

	// Create the deployment.
	selectorLabels := map[string]string{
		"app.kubernetes.io/name":     "networkrouter",
		"app.kubernetes.io/instance": req.Name,
	}

	logLevel := "info"
	if netRouter.Spec.LogLevel != "" {
		logLevel = netRouter.Spec.LogLevel
	}

	clientImage := r.ClientImage
	if netRouter.Spec.Image != "" {
		clientImage = netRouter.Spec.Image
	}

	podTemplateSpecAC := corev1ac.PodTemplateSpec().
		WithLabels(selectorLabels).
		WithSpec(corev1ac.PodSpec().
			WithTopologySpreadConstraints(
				corev1ac.TopologySpreadConstraint().
					WithMaxSkew(1).
					WithTopologyKey(corev1.LabelHostname).
					WithWhenUnsatisfiable(corev1.ScheduleAnyway).
					WithLabelSelector(metav1ac.LabelSelector().
						WithMatchLabels(selectorLabels),
					),
			).
			WithInitContainers(corev1ac.Container().
				WithName("resolv-conf").
				WithImage(clientImage).
				WithCommand("sh", "-c", "cp /etc/resolv.conf /tmp/resolv.conf && cp /etc/resolv.conf /tmp/resolv.conf.original.netbird").
				WithVolumeMounts(corev1ac.VolumeMount().
					WithName("resolv-conf").
					WithMountPath("/tmp"),
				).
				WithSecurityContext(corev1ac.SecurityContext().
					WithCapabilities(corev1ac.Capabilities().WithDrop("ALL")).
					WithReadOnlyRootFilesystem(true),
				),
			).
			WithContainers(corev1ac.Container().
				WithName("netbird").
				WithImage(clientImage).
				WithEnv(
					corev1ac.EnvVar().
						WithName("NB_SETUP_KEY").
						WithValueFrom(corev1ac.EnvVarSource().
							WithSecretKeyRef(corev1ac.SecretKeySelector().
								WithName(setupKey.SecretName()).
								WithKey(SetupKeySecretKey),
							),
						),
					corev1ac.EnvVar().
						WithName("NB_MANAGEMENT_URL").
						WithValue(r.ManagementURL),
					corev1ac.EnvVar().
						WithName("NB_LOG_LEVEL").
						WithValue(logLevel),
					corev1ac.EnvVar().
						WithName("NB_LOG_FILE").
						WithValue("console"),
					corev1ac.EnvVar().
						WithName("NB_DISABLE_PROFILES").
						WithValue("true"),
					corev1ac.EnvVar().
						WithName("NB_DISABLE_UPDATE_SETTINGS").
						WithValue("true"),
					corev1ac.EnvVar().
						WithName("NB_DAEMON_ADDR").
						WithValue("unix:///var/run/netbird/netbird.sock"),
					corev1ac.EnvVar().
						WithName("NB_ENTRYPOINT_SERVICE_TIMEOUT").
						WithValue("0"),
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
					WithCapabilities(corev1ac.Capabilities().
						WithAdd("NET_ADMIN").
						WithAdd("SYS_RESOURCE").
						WithAdd("SYS_ADMIN"),
					).
					WithPrivileged(true),
				).
				WithResources(corev1ac.ResourceRequirements().
					WithRequests(corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					}),
				),
			).
			WithVolumes(
				corev1ac.Volume().WithName("netbird-run").WithEmptyDir(corev1ac.EmptyDirVolumeSource()),
				corev1ac.Volume().WithName("netbird-lib").WithEmptyDir(corev1ac.EmptyDirVolumeSource()),
				corev1ac.Volume().WithName("ssh-etc").WithEmptyDir(corev1ac.EmptyDirVolumeSource()),
				corev1ac.Volume().WithName("resolv-conf").WithEmptyDir(corev1ac.EmptyDirVolumeSource()),
			),
		)

	replicas, workloadLabels, workloadAnnotations, err := r.resolveWorkload(netRouter, podTemplateSpecAC, selectorLabels)
	if err != nil {
		return ctrl.Result{}, err
	}

	depAC := appsv1ac.Deployment(fmt.Sprintf("networkrouter-%s", req.Name), req.Namespace).
		WithOwnerReferences(ownerRef).
		WithLabels(workloadLabels).
		WithAnnotations(workloadAnnotations).
		WithSpec(appsv1ac.DeploymentSpec().WithReplicas(replicas).WithSelector(metav1ac.LabelSelector().WithMatchLabels(selectorLabels)).WithTemplate(podTemplateSpecAC))
	err = r.Client.Apply(ctx, depAC, client.ForceOwnership)
	if err != nil {
		return ctrl.Result{}, err
	}

	if replicas > 1 {
		pdbAC := policyv1ac.PodDisruptionBudget(fmt.Sprintf("networkrouter-%s", req.Name), req.Namespace).
			WithOwnerReferences(ownerRef).
			WithLabels(workloadLabels).
			WithAnnotations(workloadAnnotations).
			WithSpec(policyv1ac.PodDisruptionBudgetSpec().
				WithMaxUnavailable(intstr.FromInt(1)).
				WithSelector(metav1ac.LabelSelector().
					WithMatchLabels(selectorLabels),
				),
			)
		err = r.Client.Apply(ctx, pdbAC, client.ForceOwnership)
		if err != nil {
			return ctrl.Result{}, err
		}
	} else {
		pdb := policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("networkrouter-%s", req.Name),
				Namespace: req.Namespace,
			},
		}
		err = r.Client.Delete(ctx, &pdb)
		if err != nil && !kerrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      *depAC.Name,
			Namespace: *depAC.Namespace,
		},
	}
	err = r.Client.Get(ctx, client.ObjectKeyFromObject(dep), dep)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if dep.Status.ReadyReplicas != dep.Status.Replicas {
		return ctrl.Result{}, nil
	}

	conditions.MarkTrue(netRouter, nbv1alpha1.ReadyCondition, nbv1alpha1.ReconciledReason, "")
	err = sp.Patch(ctx, netRouter, patch.WithStatusObservedGeneration{})
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// resolveWorkload applies the router's WorkloadOverride: it merges any
// PodTemplate override into podTemplateSpecAC in place and returns the resolved
// replica count and the workload labels/annotations (with selectorLabels folded
// into the labels).
func (r *NetworkRouterReconciler) resolveWorkload(netRouter *nbv1alpha1.NetworkRouter, podTemplateSpecAC *corev1ac.PodTemplateSpecApplyConfiguration, selectorLabels map[string]string) (int32, map[string]string, map[string]string, error) {
	workloadLabels := map[string]string{}
	workloadAnnotations := map[string]string{}
	replicas := int32(3)
	if o := netRouter.Spec.WorkloadOverride; o != nil {
		if o.Labels != nil {
			workloadLabels = o.Labels
		}
		if o.Annotations != nil {
			workloadAnnotations = o.Annotations
		}
		if o.Replicas != nil {
			replicas = *o.Replicas
		}
		if o.PodTemplate != nil {
			if err := mergePodTemplateOverride(podTemplateSpecAC, o.PodTemplate); err != nil {
				return 0, nil, nil, err
			}
		}
	}
	maps.Copy(workloadLabels, selectorLabels)
	return replicas, workloadLabels, workloadAnnotations, nil
}

// mergePodTemplateOverride strategic-merges override onto dst (the generated pod
// template) in place.
func mergePodTemplateOverride(dst *corev1ac.PodTemplateSpecApplyConfiguration, override any) error {
	baseJSON, err := json.Marshal(dst)
	if err != nil {
		return err
	}
	overrideJSON, err := json.Marshal(override)
	if err != nil {
		return err
	}
	mergedJSON, err := strategicpatch.StrategicMergePatch(baseJSON, overrideJSON, corev1.PodTemplateSpec{})
	if err != nil {
		return err
	}
	return json.Unmarshal(mergedJSON, dst)
}

// upsertNetwork creates or updates the NetBird network backing the router,
// returning its ID.
func (r *NetworkRouterReconciler) upsertNetwork(ctx context.Context, netRouter *nbv1alpha1.NetworkRouter) (string, error) {
	networkReq := api.NetworkRequest{
		Name: netRouter.Name,
	}
	if netRouter.Status.NetworkID != "" {
		_, err := r.Netbird.Networks.Get(ctx, netRouter.Status.NetworkID)
		switch {
		case err == nil:
			networkResp, err := r.Netbird.Networks.Update(ctx, netRouter.Status.NetworkID, networkReq)
			if err != nil {
				return "", err
			}
			return networkResp.Id, nil
		case !netbird.IsNotFound(err):
			return "", err
		}
		// Not found (deleted out of band) — fall through to create.
	}
	networkResp, err := r.Netbird.Networks.Create(ctx, networkReq)
	if err != nil {
		return "", err
	}
	return networkResp.Id, nil
}

// upsertRoutingPeer creates or updates the network's routing peer (bound to the
// router's peer group), returning its ID.
func (r *NetworkRouterReconciler) upsertRoutingPeer(ctx context.Context, networkID, existingID, groupID string) (string, error) {
	routerReq := api.NetworkRouterRequest{
		Enabled:    true,
		Masquerade: true,
		Metric:     9999,
		PeerGroups: new([]string{groupID}),
	}
	routers := r.Netbird.Networks.Routers(networkID)
	if existingID != "" {
		_, err := routers.Get(ctx, existingID)
		switch {
		case err == nil:
			resp, err := routers.Update(ctx, existingID, routerReq)
			if err != nil {
				return "", err
			}
			return resp.Id, nil
		case !netbird.IsNotFound(err):
			return "", err
		}
		// Not found (deleted out of band) — fall through to create.
	}
	resp, err := routers.Create(ctx, routerReq)
	if err != nil {
		return "", err
	}
	return resp.Id, nil
}

// reconcileServiceCIDRs ensures one NetBird subnet resource exists per
// spec.ServiceCIDRs entry in the router's network, creating new ones, keeping
// existing ones, and deleting resources for CIDRs that were removed. The
// resource type (subnet) is derived by NetBird from the CIDR address. Resources
// are created without groups, matching the per-service resources the HTTPRoute
// controller creates; routing for reverse-proxy targets does not depend on
// resource group membership.
func (r *NetworkRouterReconciler) reconcileServiceCIDRs(ctx context.Context, sp *patch.SerialPatcher, netRouter *nbv1alpha1.NetworkRouter, networkID string) error {
	groupIDs, err := netbirdutil.GetGroupIDs(ctx, r.Client, r.Netbird, netRouter.Spec.ResourceGroups, netRouter.Namespace)
	if err != nil {
		return err
	}

	existing := map[string]string{}
	for _, rec := range netRouter.Status.ServiceCIDRResources {
		existing[rec.CIDR] = rec.ResourceID
	}

	desired := map[string]bool{}
	kept := make([]nbv1alpha1.ServiceCIDRResource, 0, len(netRouter.Spec.ServiceCIDRs))
	for _, cidr := range netRouter.Spec.ServiceCIDRs {
		desired[cidr] = true
		req := api.NetworkResourceRequest{
			Name:        fmt.Sprintf("%s-%s", netRouter.Name, cidrResourceSuffix(cidr)),
			Description: new("service CIDR routed by " + netRouter.Name),
			Address:     cidr,
			Enabled:     true,
			Groups:      groupIDs,
		}
		if id, ok := existing[cidr]; ok {
			_, err := r.Netbird.Networks.Resources(networkID).Get(ctx, id)
			switch {
			case err == nil:
				if _, err := r.Netbird.Networks.Resources(networkID).Update(ctx, id, req); err != nil {
					return err
				}
				kept = append(kept, nbv1alpha1.ServiceCIDRResource{CIDR: cidr, ResourceID: id})
				continue
			case !netbird.IsNotFound(err):
				return err
			}
			// Tracked resource is gone (deleted out of band) — fall through and recreate it.
		}
		resp, err := r.Netbird.Networks.Resources(networkID).Create(ctx, req)
		if err != nil {
			return err
		}
		kept = append(kept, nbv1alpha1.ServiceCIDRResource{CIDR: cidr, ResourceID: resp.Id})
	}

	// Delete resources for CIDRs no longer in spec.
	for cidr, id := range existing {
		if !desired[cidr] {
			if err := r.Netbird.Networks.Resources(networkID).Delete(ctx, id); err != nil && !netbird.IsNotFound(err) {
				return err
			}
		}
	}

	netRouter.Status.ServiceCIDRResources = kept
	return sp.Patch(ctx, netRouter)
}

// cidrResourceSuffix turns a CIDR into a name-safe suffix for the resource.
func cidrResourceSuffix(cidr string) string {
	return strings.NewReplacer("/", "-", ":", "-", ".", "-").Replace(cidr)
}

func (r *NetworkRouterReconciler) reconcileDelete(ctx context.Context, sp *patch.SerialPatcher, netRouter *nbv1alpha1.NetworkRouter) (ctrl.Result, error) {
	if netRouter.Status.NetworkID != "" && netRouter.Status.RoutingPeerID != "" {
		err := r.Netbird.Networks.Routers(netRouter.Status.NetworkID).Delete(ctx, netRouter.Status.RoutingPeerID)
		if err != nil && !netbird.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	if netRouter.Status.NetworkID != "" {
		err := r.Netbird.Networks.Delete(ctx, netRouter.Status.NetworkID)
		if err != nil && !netbird.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(netRouter, k8sutil.Finalizer("networkrouter"))
	err := sp.Patch(ctx, netRouter)
	if err != nil {
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
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
