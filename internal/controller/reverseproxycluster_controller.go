// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/patch"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/k8sutil"
	"github.com/netbirdio/kubernetes-operator/internal/netbirdutil"
	"github.com/netbirdio/kubernetes-operator/internal/version"
	nbv1alpha1ac "github.com/netbirdio/kubernetes-operator/pkg/applyconfigurations/api/v1alpha1"
)

const (
	proxyTokenKey = "token"
	// proxyListenPort is the proxy's single SNI/HTTP listener — a non-privileged
	// port so the container needs no NET_BIND_SERVICE. The LoadBalancer Service
	// maps the public 80/443 onto it.
	proxyListenPort = 8443
	proxyHealthPort = 8080
)

// ReverseProxyClusterReconciler deploys and enrolls a NetBird bring-your-own
// reverse proxy and registers it as the account's own proxy cluster. It owns the
// token Secret, the proxy Deployment + LoadBalancer Service, and the DNSZone +
// DNSRecords (the proxy's A record and a catch-all) — composing the existing
// CRDs. The proxy is reached over the mesh via its LoadBalancer IP.
type ReverseProxyClusterReconciler struct {
	client.Client

	Netbird       *netbird.Client
	ManagementURL string
	Recorder      record.EventRecorder
}

// +kubebuilder:rbac:groups=netbird.io,resources=reverseproxyclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=netbird.io,resources=reverseproxyclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=netbird.io,resources=reverseproxyclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

func (r *ReverseProxyClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	rpc := &nbv1alpha1.ReverseProxyCluster{}
	if err := r.Get(ctx, req.NamespacedName, rpc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ctrl.LoggerFrom(ctx).V(1).Info("reconciling reverse proxy cluster")
	sp := patch.NewSerialPatcher(rpc, r.Client)

	if !rpc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, sp, rpc)
	}

	controllerutil.AddFinalizer(rpc, k8sutil.Finalizer("reverseproxycluster"))
	if err := sp.Patch(ctx, rpc); err != nil {
		return ctrl.Result{}, err
	}

	ownerRef, err := k8sutil.ControllerReference(rpc, r.Scheme())
	if err != nil {
		return ctrl.Result{}, err
	}

	// 1. Mint the proxy enrollment token once (gated on status.tokenID), storing
	//    the one-shot plain token in an owned Secret.
	if rpc.Status.TokenID == "" {
		resp, err := r.Netbird.ReverseProxyTokens.Create(ctx, api.ProxyTokenRequest{
			Name: fmt.Sprintf("k8s-%s-%s", rpc.Namespace, rpc.Name),
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		secretAC := corev1ac.Secret(proxyResourceName(rpc), rpc.Namespace).
			WithOwnerReferences(ownerRef).
			WithStringData(map[string]string{proxyTokenKey: resp.PlainToken})
		if err := r.Apply(ctx, secretAC, client.ForceOwnership); err != nil {
			return ctrl.Result{}, err
		}
		rpc.Status.TokenID = resp.Id
		if err := sp.Patch(ctx, rpc); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 2. Ensure the DNS zone the proxy fronts (unless an existing one is referenced).
	zoneRef, err := r.ensureZone(ctx, rpc, ownerRef)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 3. Deploy the proxy and its LoadBalancer Service.
	if err := r.applyDeployment(ctx, rpc, ownerRef); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyService(ctx, rpc, ownerRef); err != nil {
		return ctrl.Result{}, err
	}

	// 4. Wait for the LoadBalancer IP, then publish the proxy A record + catch-all.
	ip, ok := r.serviceIP(ctx, rpc)
	if !ok {
		conditions.MarkFalse(rpc, nbv1alpha1.ReadyCondition, nbv1alpha1.DependencyReason, "waiting for the proxy LoadBalancer IP")
		if err := sp.Patch(ctx, rpc); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: dependencyRetry}, nil
	}
	rpc.Status.LoadBalancerIP = ip
	if err := r.applyRecords(ctx, rpc, ownerRef, zoneRef, ip); err != nil {
		return ctrl.Result{}, err
	}

	// 5. Ready once the proxy has enrolled (its cluster resolves at the address).
	cluster, err := netbirdutil.GetProxyClusterByAddress(ctx, r.Netbird, rpc.Spec.ClusterAddress)
	if err != nil {
		conditions.MarkFalse(rpc, nbv1alpha1.ReadyCondition, nbv1alpha1.DependencyReason, "waiting for the proxy to enroll at %s", rpc.Spec.ClusterAddress)
		if err := sp.Patch(ctx, rpc); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: dependencyRetry}, nil
	}
	rpc.Status.ClusterAddress = rpc.Spec.ClusterAddress
	// Surface the proxy's connectivity (embedded client heartbeat) for visibility.
	rpc.Status.Online = cluster.Online
	rpc.Status.ConnectedProxies = cluster.ConnectedProxies

	// 6. Register the custom domain (Domain -> this cluster) so service domains
	//    under it derive the cluster. Re-derive the id from the live list every
	//    reconcile (adopt an existing registration, else create) so an out-of-band
	//    deletion self-heals within the resync window instead of getting stuck
	//    validating a registration that no longer exists.
	domainID, err := r.ensureDomain(ctx, rpc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if rpc.Status.DomainID != domainID {
		rpc.Status.DomainID = domainID
		if err := sp.Patch(ctx, rpc); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Trigger/confirm validation; requeue until it passes (DNS may still be settling).
	if err := r.Netbird.ReverseProxyDomains.Validate(ctx, rpc.Status.DomainID); err != nil {
		conditions.MarkFalse(rpc, nbv1alpha1.ReadyCondition, nbv1alpha1.DependencyReason, "validating custom domain %s: %s", rpc.Spec.Domain, err.Error())
		if perr := sp.Patch(ctx, rpc); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{RequeueAfter: dependencyRetry}, nil
	}

	conditions.MarkTrue(rpc, nbv1alpha1.ReadyCondition, nbv1alpha1.ReconciledReason, "")
	if err := sp.Patch(ctx, rpc, patch.WithStatusObservedGeneration{}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

func (r *ReverseProxyClusterReconciler) reconcileDelete(ctx context.Context, sp *patch.SerialPatcher, rpc *nbv1alpha1.ReverseProxyCluster) (ctrl.Result, error) {
	// Revoke the token; owned children (Secret/Deployment/Service/DNS*) GC via
	// owner refs. The custom domain and the account cluster registration are keyed
	// on this cluster's Domain/ClusterAddress and may be shared with another
	// ReverseProxyCluster fronting the same domain — only drop them when no
	// surviving CR still references that domain, so deleting one doesn't pull the
	// registration out from under the other.
	if rpc.Status.TokenID != "" {
		if err := r.Netbird.ReverseProxyTokens.Delete(ctx, rpc.Status.TokenID); err != nil && !netbird.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	shared, err := r.domainSharedByOtherCluster(ctx, rpc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !shared {
		if rpc.Status.DomainID != "" {
			if err := r.Netbird.ReverseProxyDomains.Delete(ctx, rpc.Status.DomainID); err != nil && !netbird.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		if rpc.Status.ClusterAddress != "" {
			if err := r.Netbird.ReverseProxyClusters.Delete(ctx, rpc.Status.ClusterAddress); err != nil && !netbird.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}
	controllerutil.RemoveFinalizer(rpc, k8sutil.Finalizer("reverseproxycluster"))
	if err := sp.Patch(ctx, rpc); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// ensureDomain returns the NetBird custom-domain id for rpc.Spec.Domain, adopting
// an existing registration or creating one. Listing every reconcile (rather than
// gating on a recorded id) lets an out-of-band deletion self-heal.
func (r *ReverseProxyClusterReconciler) ensureDomain(ctx context.Context, rpc *nbv1alpha1.ReverseProxyCluster) (string, error) {
	domains, err := r.Netbird.ReverseProxyDomains.List(ctx)
	if err != nil {
		return "", err
	}
	for _, d := range domains {
		if d.Domain == rpc.Spec.Domain {
			return d.Id, nil
		}
	}
	resp, err := r.Netbird.ReverseProxyDomains.Create(ctx, api.ReverseProxyDomainRequest{
		Domain:        rpc.Spec.Domain,
		TargetCluster: rpc.Spec.ClusterAddress,
	})
	if err != nil {
		return "", err
	}
	return resp.Id, nil
}

// domainSharedByOtherCluster reports whether another (non-deleting)
// ReverseProxyCluster CR fronts the same Domain, so this CR's deletion must not
// drop the shared NetBird custom-domain / cluster registration.
func (r *ReverseProxyClusterReconciler) domainSharedByOtherCluster(ctx context.Context, rpc *nbv1alpha1.ReverseProxyCluster) (bool, error) {
	var list nbv1alpha1.ReverseProxyClusterList
	if err := r.List(ctx, &list); err != nil {
		return false, err
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.UID == rpc.UID || !other.DeletionTimestamp.IsZero() {
			continue
		}
		if other.Spec.Domain == rpc.Spec.Domain {
			return true, nil
		}
	}
	return false, nil
}

// ensureZone applies an owned DNSZone for spec.Domain, or returns the referenced
// zone when spec.ZoneRef is set. The returned ref is what the DNSRecords target.
func (r *ReverseProxyClusterReconciler) ensureZone(ctx context.Context, rpc *nbv1alpha1.ReverseProxyCluster, ownerRef *metav1ac.OwnerReferenceApplyConfiguration) (*nbv1alpha1.CrossNamespaceReference, error) {
	if rpc.Spec.ZoneRef != nil {
		return rpc.Spec.ZoneRef, nil
	}
	zoneAC := nbv1alpha1ac.DNSZone(proxyResourceName(rpc), rpc.Namespace).
		WithOwnerReferences(ownerRef).
		WithSpec(nbv1alpha1ac.DNSZoneSpec().
			WithName(rpc.Spec.Domain).
			WithDomain(rpc.Spec.Domain).
			WithEnabled(true).
			WithDistributionGroups(groupRefACs(rpc.Spec.Groups)...))
	if err := r.Apply(ctx, zoneAC, client.ForceOwnership); err != nil {
		return nil, err
	}
	return &nbv1alpha1.CrossNamespaceReference{Name: proxyResourceName(rpc), Namespace: rpc.Namespace}, nil
}

// applyRecords publishes the proxy's A record (clusterAddress -> LB IP) and a
// catch-all CNAME (*.domain -> clusterAddress) so any service hostname resolves
// to the proxy. NetBird verifies the A record before treating the proxy ready.
func (r *ReverseProxyClusterReconciler) applyRecords(ctx context.Context, rpc *nbv1alpha1.ReverseProxyCluster, ownerRef *metav1ac.OwnerReferenceApplyConfiguration, zoneRef *nbv1alpha1.CrossNamespaceReference, ip string) error {
	zoneRefAC := nbv1alpha1ac.CrossNamespaceReference().WithName(zoneRef.Name).WithNamespace(zoneRef.Namespace)

	aRecord := nbv1alpha1ac.DNSRecord(proxyResourceName(rpc)+"-a", rpc.Namespace).
		WithOwnerReferences(ownerRef).
		WithSpec(nbv1alpha1ac.DNSRecordSpec().
			WithZoneRef(zoneRefAC).
			WithName(rpc.Spec.ClusterAddress).
			WithType("A").
			WithContent(ip).
			WithTTL(int(dnsRecordTTL.Seconds())))
	if err := r.Apply(ctx, aRecord, client.ForceOwnership); err != nil {
		return err
	}

	catchAll := nbv1alpha1ac.DNSRecord(proxyResourceName(rpc)+"-catchall", rpc.Namespace).
		WithOwnerReferences(ownerRef).
		WithSpec(nbv1alpha1ac.DNSRecordSpec().
			WithZoneRef(zoneRefAC).
			WithName("*." + rpc.Spec.Domain).
			WithType("CNAME").
			WithContent(rpc.Spec.ClusterAddress).
			WithTTL(int(dnsRecordTTL.Seconds())))
	return r.Apply(ctx, catchAll, client.ForceOwnership)
}

func (r *ReverseProxyClusterReconciler) applyDeployment(ctx context.Context, rpc *nbv1alpha1.ReverseProxyCluster, ownerRef *metav1ac.OwnerReferenceApplyConfiguration) error {
	image := rpc.Spec.Image
	if image == "" {
		image = version.ReverseProxyImage
	}
	replicas := int32(1)
	if rpc.Spec.Replicas != nil {
		replicas = *rpc.Spec.Replicas
	}
	labels := proxySelectorLabels(rpc)

	container := corev1ac.Container().
		WithName("proxy").
		WithImage(image).
		WithPorts(
			corev1ac.ContainerPort().WithName("https").WithContainerPort(proxyListenPort),
			corev1ac.ContainerPort().WithName("health").WithContainerPort(proxyHealthPort),
		).
		WithEnv(
			corev1ac.EnvVar().WithName("NB_PROXY_MANAGEMENT_ADDRESS").WithValue(r.ManagementURL),
			corev1ac.EnvVar().WithName("NB_PROXY_DOMAIN").WithValue(rpc.Spec.ClusterAddress),
			corev1ac.EnvVar().WithName("NB_PROXY_ADDRESS").WithValue(fmt.Sprintf(":%d", proxyListenPort)),
			corev1ac.EnvVar().WithName("NB_PROXY_HEALTH_ADDRESS").WithValue(fmt.Sprintf(":%d", proxyHealthPort)),
			corev1ac.EnvVar().WithName("NB_PROXY_ACME_CERTIFICATES").WithValue("false"),
			corev1ac.EnvVar().WithName("NB_PROXY_CERTIFICATE_DIRECTORY").WithValue("/certs"),
			// Run an embedded netbird client (userspace WG) so the cluster can serve
			// NetBird-Only (private) services.
			corev1ac.EnvVar().WithName("NB_PROXY_PRIVATE").WithValue(strconv.FormatBool(rpc.Spec.Private)),
			corev1ac.EnvVar().WithName("NB_PROXY_TOKEN").WithValueFrom(corev1ac.EnvVarSource().
				WithSecretKeyRef(corev1ac.SecretKeySelector().WithName(proxyResourceName(rpc)).WithKey(proxyTokenKey))),
		).
		WithStartupProbe(corev1ac.Probe().
			WithHTTPGet(corev1ac.HTTPGetAction().WithPath("/healthz/startup").WithPort(intstr.FromInt(proxyHealthPort))).
			WithFailureThreshold(30).WithPeriodSeconds(2)).
		WithReadinessProbe(corev1ac.Probe().
			WithHTTPGet(corev1ac.HTTPGetAction().WithPath("/healthz/ready").WithPort(intstr.FromInt(proxyHealthPort)))).
		WithLivenessProbe(corev1ac.Probe().
			WithHTTPGet(corev1ac.HTTPGetAction().WithPath("/healthz/live").WithPort(intstr.FromInt(proxyHealthPort)))).
		WithSecurityContext(corev1ac.SecurityContext().
			WithAllowPrivilegeEscalation(false).
			WithCapabilities(corev1ac.Capabilities().WithDrop("ALL"))).
		WithResources(corev1ac.ResourceRequirements().
			WithRequests(corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			}))

	// Optional log level for the proxy and its embedded netbird client. Set both
	// envs so it applies whichever the image honors — e.g. "error" silences the
	// embedded client's unused P2P/ICE warnings on a centralised cluster.
	if rpc.Spec.LogLevel != "" {
		container.WithEnv(
			corev1ac.EnvVar().WithName("NB_PROXY_LOG_LEVEL").WithValue(rpc.Spec.LogLevel),
			corev1ac.EnvVar().WithName("NB_LOG_LEVEL").WithValue(rpc.Spec.LogLevel),
		)
	}

	// Mount the cert-manager TLS Secret as the proxy's static certificate. The
	// mount must be added to the container before WithContainers, which copies it.
	if rpc.Spec.CertSecretName != "" {
		container.WithVolumeMounts(corev1ac.VolumeMount().WithName("certs").WithMountPath("/certs").WithReadOnly(true))
	}
	podSpec := corev1ac.PodSpec().
		WithContainers(container).
		// ndots:1 so the proxy resolves external FQDNs (pkgs.netbird.io for the
		// GeoLite2 DB, ACME, etc.) as-is instead of appending the NetBird search
		// domain kubelet injects — which otherwise hijacks them and breaks egress
		// (tls: internal error -> no geo DB -> the proxy denies all requests 403).
		// Cluster names (svc.cluster.local) have >1 dot, so they still resolve.
		WithDNSConfig(corev1ac.PodDNSConfig().
			WithOptions(corev1ac.PodDNSConfigOption().WithName("ndots").WithValue("1")))
	if rpc.Spec.CertSecretName != "" {
		podSpec.WithVolumes(corev1ac.Volume().WithName("certs").
			WithSecret(corev1ac.SecretVolumeSource().WithSecretName(rpc.Spec.CertSecretName)))
	}

	depAC := appsv1ac.Deployment(proxyResourceName(rpc), rpc.Namespace).
		WithOwnerReferences(ownerRef).
		WithLabels(labels).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(replicas).
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(labels)).
			WithTemplate(corev1ac.PodTemplateSpec().WithLabels(labels).WithSpec(podSpec)))
	return r.Apply(ctx, depAC, client.ForceOwnership)
}

func (r *ReverseProxyClusterReconciler) applyService(ctx context.Context, rpc *nbv1alpha1.ReverseProxyCluster, ownerRef *metav1ac.OwnerReferenceApplyConfiguration) error {
	svcAC := corev1ac.Service(proxyResourceName(rpc), rpc.Namespace).
		WithOwnerReferences(ownerRef).
		WithSpec(corev1ac.ServiceSpec().
			WithType(corev1.ServiceTypeLoadBalancer).
			// Dual-stack where the cluster supports it, so the LoadBalancer
			// controller advertises both A and AAAA for the proxy. PreferDualStack
			// degrades to single-stack on IPv4-only clusters.
			WithIPFamilyPolicy(corev1.IPFamilyPolicyPreferDualStack).
			WithSelector(proxySelectorLabels(rpc)).
			WithPorts(
				// Public 80 and 443 both map onto the proxy's single listener,
				// which detects TLS vs plain HTTP (SNI router).
				corev1ac.ServicePort().WithName("http").WithPort(80).WithTargetPort(intstr.FromInt(proxyListenPort)),
				corev1ac.ServicePort().WithName("https").WithPort(443).WithTargetPort(intstr.FromInt(proxyListenPort)),
			))
	if len(rpc.Spec.ServiceAnnotations) > 0 {
		svcAC.WithAnnotations(rpc.Spec.ServiceAnnotations)
	}
	// Join the proxy resource to the configured groups so the LoadBalancer
	// controller advertises it into them (mesh routability + zone distribution).
	if names := groupNames(rpc.Spec.Groups); names != "" {
		svcAC.WithAnnotations(map[string]string{groupsAnnotation: names})
	}
	return r.Apply(ctx, svcAC, client.ForceOwnership)
}

// serviceIP returns the proxy Service's first LoadBalancer ingress IP, or false
// while none is assigned yet.
func (r *ReverseProxyClusterReconciler) serviceIP(ctx context.Context, rpc *nbv1alpha1.ReverseProxyCluster) (string, bool) {
	svc := &corev1.Service{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: rpc.Namespace, Name: proxyResourceName(rpc)}, svc); err != nil {
		return "", false
	}
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			return ing.IP, true
		}
	}
	return "", false
}

func (r *ReverseProxyClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nbv1alpha1.ReverseProxyCluster{}).
		WithLogConstructor(logConstructor(mgr, "ReverseProxyCluster")).
		Owns(&corev1.Secret{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&nbv1alpha1.DNSZone{}).
		Owns(&nbv1alpha1.DNSRecord{}).
		Complete(r)
}

func proxyResourceName(rpc *nbv1alpha1.ReverseProxyCluster) string {
	return "reverseproxycluster-" + rpc.Name
}

func proxySelectorLabels(rpc *nbv1alpha1.ReverseProxyCluster) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "netbird-reverse-proxy",
		"app.kubernetes.io/instance": rpc.Name,
	}
}

// groupRefACs maps GroupReferences onto their apply-config form.
func groupRefACs(groups []nbv1alpha1.GroupReference) []*nbv1alpha1ac.GroupReferenceApplyConfiguration {
	out := make([]*nbv1alpha1ac.GroupReferenceApplyConfiguration, 0, len(groups))
	for _, g := range groups {
		ref := nbv1alpha1ac.GroupReference()
		switch {
		case g.ID != nil:
			ref.WithID(*g.ID)
		case g.Name != nil:
			ref.WithName(*g.Name)
		case g.LocalRef != nil:
			ref.WithLocalRef(*g.LocalRef)
		}
		out = append(out, ref)
	}
	return out
}

// groupNames joins the named groups into the comma-separated netbird.io/groups
// annotation value (id/localRef groups are skipped — the annotation is by name).
func groupNames(groups []nbv1alpha1.GroupReference) string {
	names := make([]string, 0, len(groups))
	for _, g := range groups {
		if g.Name != nil {
			names = append(names, *g.Name)
		}
	}
	return strings.Join(names, ",")
}
