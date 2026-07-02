// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/netbirdmock"
)

var _ = Describe("LoadBalancer-IP translation", func() {
	ctx := context.Background()

	var (
		nbClient *netbird.Client
		controls *netbirdmock.Controls
		ns       string
	)

	BeforeEach(func() {
		nbClient, controls = netbirdmock.ClientWithControls()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "lb-"}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		ns = nsObj.Name
	})

	reconcileOnce := func(r interface {
		Reconcile(context.Context, reconcile.Request) (reconcile.Result, error)
	}, name string) (reconcile.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKey{Name: name, Namespace: ns}})
	}

	// readyNetwork creates a Network (named after the namespace, so it's unique)
	// and reconciles it to Ready.
	readyNetwork := func() *nbv1alpha1.Network {
		network := &nbv1alpha1.Network{
			ObjectMeta: metav1.ObjectMeta{Name: ns, Namespace: ns},
			Spec:       nbv1alpha1.NetworkSpec{Name: ns},
		}
		Expect(k8sClient.Create(ctx, network)).To(Succeed())
		_, err := reconcileOnce(NewNetworkReconciler(k8sClient, nbClient, nil), ns)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(network), network)).To(Succeed())
		Expect(network.Status.NetworkID).NotTo(BeEmpty())
		return network
	}

	// lbZoneDomain is the apex domain the LoadBalancer controller is configured
	// with; it creates and owns the DNSZone itself (named after the domain).
	const lbZoneDomain = "kube.example.com"

	// lbService creates a Service type=LoadBalancer and sets its ingress IPs.
	lbService := func(name string, ips ...string) *corev1.Service {
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
			},
			Spec: corev1.ServiceSpec{
				Type:  corev1.ServiceTypeLoadBalancer,
				Ports: []corev1.ServicePort{{Port: 80}},
			},
		}
		Expect(k8sClient.Create(ctx, svc)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), svc)).To(Succeed())
		for _, ip := range ips {
			svc.Status.LoadBalancer.Ingress = append(svc.Status.LoadBalancer.Ingress, corev1.LoadBalancerIngress{IP: ip})
		}
		Expect(k8sClient.Status().Update(ctx, svc)).To(Succeed())
		return svc
	}

	Describe("NetworkRouter", func() {
		It("group mode binds an existing group to the network", func() {
			network := readyNetwork()

			grp := &nbv1alpha1.Group{
				ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: ns},
				Spec:       nbv1alpha1.GroupSpec{Name: ns + "-nodes"},
			}
			Expect(k8sClient.Create(ctx, grp)).To(Succeed())
			_, err := reconcileOnce(&GroupReconciler{Client: k8sClient, Netbird: nbClient}, "nodes")
			Expect(err).NotTo(HaveOccurred())

			nr := &nbv1alpha1.NetworkRouter{
				ObjectMeta: metav1.ObjectMeta{Name: "router", Namespace: ns},
				Spec: nbv1alpha1.NetworkRouterSpec{
					NetworkRef: nbv1alpha1.CrossNamespaceReference{Name: ns, Namespace: ns},
					Peers:      nbv1alpha1.NetworkRouterPeers{Group: &nbv1alpha1.GroupReference{LocalRef: &corev1.LocalObjectReference{Name: "nodes"}}},
					Masquerade: true,
					Metric:     9999,
					Enabled:    true,
				},
			}
			Expect(k8sClient.Create(ctx, nr)).To(Succeed())
			_, err = reconcileOnce(&NetworkRouterReconciler{Client: k8sClient, Netbird: nbClient}, "router")
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(nr), nr)).To(Succeed())
			Expect(nr.Status.RouterID).NotTo(BeEmpty())
			Expect(meta.IsStatusConditionTrue(nr.Status.Conditions, nbv1alpha1.ReadyCondition)).To(BeTrue())

			routers, err := nbClient.Networks.Routers(network.Status.NetworkID).List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(routers).To(HaveLen(1))
		})

		It("deploy mode creates a hostNetwork DaemonSet and joins the network once ready", func() {
			network := readyNetwork()

			nr := &nbv1alpha1.NetworkRouter{
				ObjectMeta: metav1.ObjectMeta{Name: "nr", Namespace: ns},
				Spec: nbv1alpha1.NetworkRouterSpec{
					NetworkRef: nbv1alpha1.CrossNamespaceReference{Name: ns, Namespace: ns},
					Peers:      nbv1alpha1.NetworkRouterPeers{Deploy: &nbv1alpha1.RouterDeploy{}},
					Masquerade: true,
					Metric:     9999,
					Enabled:    true,
				},
			}
			Expect(k8sClient.Create(ctx, nr)).To(Succeed())
			nrRec := &NetworkRouterReconciler{Client: k8sClient, Netbird: nbClient, ClientImage: "netbird:latest", ManagementURL: "https://netbird.io"}

			// Pass 1: creates the router Group; waits on it.
			_, err := reconcileOnce(nrRec, "nr")
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileOnce(&GroupReconciler{Client: k8sClient, Netbird: nbClient}, "nr-router")
			Expect(err).NotTo(HaveOccurred())
			// Pass 2: creates the SetupKey; waits on it.
			_, err = reconcileOnce(nrRec, "nr")
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileOnce(&SetupKeyReconciler{Client: k8sClient, Netbird: nbClient}, "nr-router")
			Expect(err).NotTo(HaveOccurred())
			// Pass 3: creates the DaemonSet; waits on its readiness.
			_, err = reconcileOnce(nrRec, "nr")
			Expect(err).NotTo(HaveOccurred())

			ds := &appsv1.DaemonSet{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "nr-router", Namespace: ns}, ds)).To(Succeed())
			Expect(ds.Spec.Template.Spec.HostNetwork).To(BeTrue())
			Expect(ds.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(ds.Spec.Template.Spec.Containers[0].Image).To(Equal("netbird:latest"))

			// Fake the router pods becoming ready (no kubelet in envtest).
			ds.Status.DesiredNumberScheduled = 1
			ds.Status.NumberReady = 1
			Expect(k8sClient.Status().Update(ctx, ds)).To(Succeed())

			// Final pass: joins the peers to the network and goes Ready.
			_, err = reconcileOnce(nrRec, "nr")
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(nr), nr)).To(Succeed())
			Expect(nr.Status.RouterID).NotTo(BeEmpty())
			Expect(meta.IsStatusConditionTrue(nr.Status.Conditions, nbv1alpha1.ReadyCondition)).To(BeTrue())

			routers, err := nbClient.Networks.Routers(network.Status.NetworkID).List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(routers).To(HaveLen(1))
		})
	})

	Describe("LoadBalancer translation", func() {
		It("advertises a dualstack LB Service as NetworkResource + DNSRecord per family", func() {
			readyNetwork()
			svc := lbService("app", "192.0.2.10", "2001:db8::10")

			r := &LoadBalancerReconciler{Client: k8sClient, Namespace: ns, DefaultAdvertise: true, Network: ns, DNSZone: lbZoneDomain, DNSZoneGroups: []string{"All"}, DefaultGroups: []string{"All"}}
			_, err := reconcileOnce(r, "app")
			Expect(err).NotTo(HaveOccurred())

			// The operator creates and owns the LoadBalancer DNSZone (named after
			// the domain), with the configured distribution groups.
			zone := &nbv1alpha1.DNSZone{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: lbZoneDomain, Namespace: ns}, zone)).To(Succeed())
			Expect(zone.Spec.Domain).To(Equal(lbZoneDomain))
			Expect(zone.Spec.Enabled).To(BeTrue())
			Expect(zone.Spec.DistributionGroups).To(HaveLen(1))
			Expect(zone.Spec.DistributionGroups[0].Name).To(HaveValue(Equal("All")))

			v4 := &nbv1alpha1.NetworkResource{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "app-ipv4", Namespace: ns}, v4)).To(Succeed())
			Expect(v4.Spec.Address).To(Equal("192.0.2.10"))
			Expect(v4.Spec.NetworkRef.Name).To(Equal(ns))
			Expect(v4.Spec.Groups).To(HaveLen(1))
			Expect(v4.Spec.Groups[0].Name).To(HaveValue(Equal("All")))

			recV4 := &nbv1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "app-ipv4", Namespace: ns}, recV4)).To(Succeed())
			Expect(recV4.Spec.Name).To(Equal(fmt.Sprintf("app-%s.kube.example.com", ns)))
			Expect(recV4.Spec.Type).To(Equal("A"))
			Expect(recV4.Spec.Content).To(Equal("192.0.2.10"))

			recV6 := &nbv1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "app-ipv6", Namespace: ns}, recV6)).To(Succeed())
			Expect(recV6.Spec.Type).To(Equal("AAAA"))
			Expect(recV6.Spec.Content).To(Equal("2001:db8::10"))

			// Same dualstack name for both families.
			Expect(recV6.Spec.Name).To(Equal(recV4.Spec.Name))
			_ = svc
		})

		It("namespace opt-out advertises nothing", func() {
			readyNetwork()
			nsObj := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: ns}, nsObj)).To(Succeed())
			nsObj.Annotations = map[string]string{advertiseAnnotation: "false"}
			Expect(k8sClient.Update(ctx, nsObj)).To(Succeed())
			lbService("app", "192.0.2.10")

			r := &LoadBalancerReconciler{Client: k8sClient, Namespace: ns, DefaultAdvertise: true, Network: ns, DNSZone: lbZoneDomain}
			_, err := reconcileOnce(r, "app")
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, client.ObjectKey{Name: "app-ipv4", Namespace: ns}, &nbv1alpha1.NetworkResource{})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("ReverseProxyService", func() {
		It("targets the backend Service's DNSRecord FQDN", func() {
			readyNetwork()
			lbService("app", "192.0.2.10")
			// advertise the Service so its DNSRecord exists.
			_, err := reconcileOnce(&LoadBalancerReconciler{Client: k8sClient, Namespace: ns, DefaultAdvertise: true, Network: ns, DNSZone: lbZoneDomain}, "app")
			Expect(err).NotTo(HaveOccurred())

			controls.AddProxyCluster("cluster-1", "gate.test")

			rps := &nbv1alpha1.ReverseProxyService{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns},
				Spec: nbv1alpha1.ReverseProxyServiceSpec{
					Backends:     []nbv1alpha1.ReverseProxyBackend{{ServiceRef: corev1.LocalObjectReference{Name: "app"}, Path: "/"}},
					ProxyCluster: "gate.test",
					Domain:       "app.example.com",
				},
			}
			Expect(k8sClient.Create(ctx, rps)).To(Succeed())
			_, err = reconcileOnce(NewReverseProxyServiceReconciler(k8sClient, nbClient, nil), "app")
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rps), rps)).To(Succeed())
			Expect(rps.Status.ServiceID).NotTo(BeEmpty())

			services, err := nbClient.ReverseProxyServices.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(services).To(HaveLen(1))
			Expect(services[0].Domain).To(Equal("app.example.com"))
			Expect(services[0].Targets).To(HaveLen(1))
			target := services[0].Targets[0]
			Expect(target.TargetType).To(Equal(api.ServiceTargetTargetTypeCluster))
			Expect(target.TargetId).To(Equal("gate.test")) // cluster CNAME address, not the proxy-node id
			Expect(target.Host).NotTo(BeNil())
			Expect(*target.Host).To(Equal(fmt.Sprintf("app-%s.kube.example.com", ns)))
			Expect(target.Port).To(Equal(80))
			Expect(target.Options).NotTo(BeNil())
			Expect(target.Options.DirectUpstream).NotTo(BeNil())
			Expect(*target.Options.DirectUpstream).To(BeTrue())
		})

		It("exposes an L4 (tcp) service on a fixed listen port", func() {
			readyNetwork()
			// Backend with a named port (smtp) so the per-port domain reads mail-smtp.
			backend := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "mail", Namespace: ns},
				Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Ports: []corev1.ServicePort{{Name: "smtp", Port: 25}}},
			}
			Expect(k8sClient.Create(ctx, backend)).To(Succeed())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(backend), backend)).To(Succeed())
			backend.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.20"}}
			Expect(k8sClient.Status().Update(ctx, backend)).To(Succeed())
			_, err := reconcileOnce(&LoadBalancerReconciler{Client: k8sClient, Namespace: ns, DefaultAdvertise: true, Network: ns, DNSZone: lbZoneDomain}, "mail")
			Expect(err).NotTo(HaveOccurred())

			controls.AddProxyCluster("cluster-1", "gate.test")

			listen := 25
			proxyProto := true
			rps := &nbv1alpha1.ReverseProxyService{
				ObjectMeta: metav1.ObjectMeta{Name: "mail-smtp", Namespace: ns},
				Spec: nbv1alpha1.ReverseProxyServiceSpec{
					Backends:      []nbv1alpha1.ReverseProxyBackend{{ServiceRef: corev1.LocalObjectReference{Name: "mail"}, Port: 25, Path: "/"}},
					ProxyCluster:  "gate.test",
					Domain:        "mail.example.com",
					Mode:          nbv1alpha1.ReverseProxyModeTCP,
					ListenPort:    &listen,
					ProxyProtocol: &proxyProto,
				},
			}
			Expect(k8sClient.Create(ctx, rps)).To(Succeed())
			_, err = reconcileOnce(NewReverseProxyServiceReconciler(k8sClient, nbClient, nil), "mail-smtp")
			Expect(err).NotTo(HaveOccurred())

			services, err := nbClient.ReverseProxyServices.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(services).To(HaveLen(1))
			svc := services[0]
			// tcp/udp route by port; the domain is a per-port sibling named after
			// the backend port (smtp).
			Expect(svc.Domain).To(Equal("mail-smtp.example.com"))
			Expect(svc.Mode).NotTo(BeNil())
			Expect(*svc.Mode).To(Equal(api.ServiceMode(api.ServiceRequestModeTcp)))
			Expect(svc.ListenPort).NotTo(BeNil())
			Expect(*svc.ListenPort).To(Equal(25))
			Expect(svc.Targets).To(HaveLen(1))
			target := svc.Targets[0]
			Expect(target.Protocol).To(Equal(api.ServiceTargetProtocolTcp))
			Expect(target.Port).To(Equal(25))
			Expect(target.Path).To(BeNil()) // path is HTTP-only
			// proxyProtocol is mirrored onto the target so the backend sees the
			// real client IP.
			Expect(target.Options).NotTo(BeNil())
			Expect(target.Options.ProxyProtocol).NotTo(BeNil())
			Expect(*target.Options.ProxyProtocol).To(BeTrue())

			// The synthesized domain is surfaced in status for transparency.
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rps), rps)).To(Succeed())
			Expect(rps.Status.ServiceDomain).To(Equal("mail-smtp.example.com"))
		})

		It("publishes several L4 ports under one host as distinct per-port domains", func() {
			readyNetwork()
			// Multi-port mail backend with named ports.
			backend := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "mail", Namespace: ns},
				Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Ports: []corev1.ServicePort{{Name: "smtp", Port: 25}, {Name: "smtps", Port: 465}}},
			}
			Expect(k8sClient.Create(ctx, backend)).To(Succeed())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(backend), backend)).To(Succeed())
			backend.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.20"}}
			Expect(k8sClient.Status().Update(ctx, backend)).To(Succeed())
			_, err := reconcileOnce(&LoadBalancerReconciler{Client: k8sClient, Namespace: ns, DefaultAdvertise: true, Network: ns, DNSZone: lbZoneDomain}, "mail")
			Expect(err).NotTo(HaveOccurred())

			controls.AddProxyCluster("cluster-1", "gate.test")

			// One CR per port, all sharing the public host mail.example.com.
			for _, port := range []int{25, 465} {
				p := port
				rps := &nbv1alpha1.ReverseProxyService{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("mail-%d", p), Namespace: ns},
					Spec: nbv1alpha1.ReverseProxyServiceSpec{
						Backends:     []nbv1alpha1.ReverseProxyBackend{{ServiceRef: corev1.LocalObjectReference{Name: "mail"}, Port: p}},
						ProxyCluster: "gate.test",
						Domain:       "mail.example.com",
						Mode:         nbv1alpha1.ReverseProxyModeTCP,
						ListenPort:   &p,
					},
				}
				Expect(k8sClient.Create(ctx, rps)).To(Succeed())
				_, err = reconcileOnce(NewReverseProxyServiceReconciler(k8sClient, nbClient, nil), rps.Name)
				Expect(err).NotTo(HaveOccurred())
			}

			services, err := nbClient.ReverseProxyServices.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(services).To(HaveLen(2))
			domains := []string{services[0].Domain, services[1].Domain}
			// Distinct per-port sibling domains named after the backend ports.
			Expect(domains).To(ConsistOf("mail-smtp.example.com", "mail-smtps.example.com"))
		})

		It("defaults to the backend Service's first port when none is given", func() {
			readyNetwork()

			// A multi-port LoadBalancer Service (http first); the LoadBalancer
			// controller advertises it.
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "app",
					Namespace: ns,
				},
				Spec: corev1.ServiceSpec{
					Type:  corev1.ServiceTypeLoadBalancer,
					Ports: []corev1.ServicePort{{Name: "http", Port: 80}, {Name: "https", Port: 443}},
				},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), svc)).To(Succeed())
			svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.21"}}
			Expect(k8sClient.Status().Update(ctx, svc)).To(Succeed())
			_, err := reconcileOnce(&LoadBalancerReconciler{Client: k8sClient, Namespace: ns, DefaultAdvertise: true, Network: ns, DNSZone: lbZoneDomain}, "app")
			Expect(err).NotTo(HaveOccurred())

			controls.AddProxyCluster("cluster-1", "gate.test")

			rps := &nbv1alpha1.ReverseProxyService{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns},
				Spec: nbv1alpha1.ReverseProxyServiceSpec{
					// Port omitted — defaults to the Service's first port (80).
					Backends:     []nbv1alpha1.ReverseProxyBackend{{ServiceRef: corev1.LocalObjectReference{Name: "app"}, Path: "/"}},
					ProxyCluster: "gate.test",
					Domain:       "app.example.com",
				},
			}
			Expect(k8sClient.Create(ctx, rps)).To(Succeed())
			_, err = reconcileOnce(NewReverseProxyServiceReconciler(k8sClient, nbClient, nil), "app")
			Expect(err).NotTo(HaveOccurred())

			services, err := nbClient.ReverseProxyServices.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(services).To(HaveLen(1))
			Expect(services[0].Targets).To(HaveLen(1))
			Expect(services[0].Targets[0].Port).To(Equal(80)) // first port
		})

		It("targets a ClusterIP backend at its in-cluster DNS name", func() {
			controls.AddProxyCluster("cluster-1", "gate.test")

			// A plain ClusterIP backend (not advertised) — the drop-in path.
			backend := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "infisical", Namespace: ns},
				Spec: corev1.ServiceSpec{
					Type:  corev1.ServiceTypeClusterIP,
					Ports: []corev1.ServicePort{{Name: "http", Port: 8080}},
				},
			}
			Expect(k8sClient.Create(ctx, backend)).To(Succeed())

			groupAll := "All"
			rps := &nbv1alpha1.ReverseProxyService{
				ObjectMeta: metav1.ObjectMeta{Name: "secrets", Namespace: ns},
				Spec: nbv1alpha1.ReverseProxyServiceSpec{
					Backends:     []nbv1alpha1.ReverseProxyBackend{{ServiceRef: corev1.LocalObjectReference{Name: "infisical"}, Path: "/"}},
					ProxyCluster: "gate.test",
					Domain:       "secrets.ccbash.cloud",
					// Not private: access groups must be ignored, not sent (which
					// would flip the service into the NetBird-Only state).
					AccessGroups: []nbv1alpha1.GroupReference{{Name: &groupAll}},
				},
			}
			Expect(k8sClient.Create(ctx, rps)).To(Succeed())
			_, err := reconcileOnce(NewReverseProxyServiceReconciler(k8sClient, nbClient, nil), "secrets")
			Expect(err).NotTo(HaveOccurred())

			services, err := nbClient.ReverseProxyServices.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(services).To(HaveLen(1))
			Expect(services[0].Targets).To(HaveLen(1))
			target := services[0].Targets[0]
			Expect(target.Host).NotTo(BeNil())
			Expect(*target.Host).To(Equal(fmt.Sprintf("infisical.%s.svc.cluster.local", ns)))
			Expect(target.Port).To(Equal(8080))
			// Non-private: no access-group ACL is sent.
			Expect(services[0].AccessGroups).To(BeNil())
		})
	})

	Describe("ReverseProxyCluster", func() {
		It("deploys and enrolls a BYOP proxy with token, Service, DNS and readiness", func() {
			all := "All"
			rpc := &nbv1alpha1.ReverseProxyCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "gate", Namespace: ns},
				Spec: nbv1alpha1.ReverseProxyClusterSpec{
					ClusterAddress: "gate.ccbash.cloud",
					Domain:         "ccbash.cloud",
					CertSecretName: "wildcard-tls",
					Groups:         []nbv1alpha1.GroupReference{{Name: &all}},
					Private:        true,
					LogLevel:       "error",
				},
			}
			Expect(k8sClient.Create(ctx, rpc)).To(Succeed())

			r := &ReverseProxyClusterReconciler{Client: k8sClient, Netbird: nbClient, ManagementURL: "https://mgmt.test"}
			// First reconcile: token Secret, Deployment, Service, DNSZone; waits on LB IP.
			_, err := reconcileOnce(r, "gate")
			Expect(err).NotTo(HaveOccurred())

			name := "reverseproxycluster-gate"
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, secret)).To(Succeed())
			Expect(secret.Data).To(HaveKey("token"))

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, dep)).To(Succeed())
			env := dep.Spec.Template.Spec.Containers[0].Env
			Expect(envValue(env, "NB_PROXY_DOMAIN")).To(Equal("gate.ccbash.cloud"))
			Expect(envValue(env, "NB_PROXY_MANAGEMENT_ADDRESS")).To(Equal("https://mgmt.test"))
			Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("netbirdio/reverse-proxy:latest"))
			// Proxy listens on a non-privileged port; HTTP health probes on :8080.
			Expect(envValue(env, "NB_PROXY_ADDRESS")).To(Equal(":8443"))
			Expect(envValue(env, "NB_PROXY_PRIVATE")).To(Equal("true")) // embedded peer for NetBird-Only services
			// LogLevel propagates to both the proxy and its embedded netbird client.
			Expect(envValue(env, "NB_PROXY_LOG_LEVEL")).To(Equal("error"))
			Expect(envValue(env, "NB_LOG_LEVEL")).To(Equal("error"))
			// ndots:1 so external FQDNs (geo DB, ACME) resolve without the NetBird search domain.
			Expect(dep.Spec.Template.Spec.DNSConfig).NotTo(BeNil())
			Expect(dep.Spec.Template.Spec.DNSConfig.Options).To(ContainElement(corev1.PodDNSConfigOption{Name: "ndots", Value: ptrTo("1")}))
			Expect(dep.Spec.Template.Spec.Containers[0].ReadinessProbe.HTTPGet.Path).To(Equal("/healthz/ready"))

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, svc)).To(Succeed())
			Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))
			// Dual-stack so both A and AAAA get advertised for the proxy.
			Expect(svc.Spec.IPFamilyPolicy).To(HaveValue(Equal(corev1.IPFamilyPolicyPreferDualStack)))
			svcPorts := map[int32]int32{}
			for _, p := range svc.Spec.Ports {
				svcPorts[p.Port] = p.TargetPort.IntVal
			}
			Expect(svcPorts).To(HaveKeyWithValue(int32(443), int32(8443)))
			Expect(svcPorts).To(HaveKeyWithValue(int32(80), int32(8443)))

			zone := &nbv1alpha1.DNSZone{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, zone)).To(Succeed())
			Expect(zone.Spec.Domain).To(Equal("ccbash.cloud"))

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rpc), rpc)).To(Succeed())
			Expect(rpc.Status.TokenID).NotTo(BeEmpty())

			// Assign the LB IP (no cloud controller in envtest) and seed enrollment.
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), svc)).To(Succeed())
			svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.50"}}
			Expect(k8sClient.Status().Update(ctx, svc)).To(Succeed())
			controls.AddProxyCluster("c1", "gate.ccbash.cloud")

			_, err = reconcileOnce(r, "gate")
			Expect(err).NotTo(HaveOccurred())

			aRec := &nbv1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-a", Namespace: ns}, aRec)).To(Succeed())
			Expect(aRec.Spec.Type).To(Equal("A"))
			Expect(aRec.Spec.Content).To(Equal("192.0.2.50"))
			Expect(aRec.Spec.Name).To(Equal("gate.ccbash.cloud"))

			catch := &nbv1alpha1.DNSRecord{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name + "-catchall", Namespace: ns}, catch)).To(Succeed())
			Expect(catch.Spec.Type).To(Equal("CNAME"))
			Expect(catch.Spec.Name).To(Equal("*.ccbash.cloud"))
			Expect(catch.Spec.Content).To(Equal("gate.ccbash.cloud"))

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rpc), rpc)).To(Succeed())
			Expect(rpc.Status.LoadBalancerIP).To(Equal("192.0.2.50"))
			Expect(rpc.Status.ClusterAddress).To(Equal("gate.ccbash.cloud"))
			Expect(rpc.Status.DomainID).NotTo(BeEmpty()) // custom domain registered
			// Proxy connectivity surfaced from the Management API.
			Expect(rpc.Status.Online).To(BeTrue())
			Expect(rpc.Status.ConnectedProxies).To(Equal(1))
			Expect(meta.IsStatusConditionTrue(rpc.Status.Conditions, nbv1alpha1.ReadyCondition)).To(BeTrue())

			// Custom domain deleted out of band: the next reconcile re-registers it
			// (re-derived from the live list each pass) instead of staying stuck
			// validating a registration that no longer exists.
			oldDomainID := rpc.Status.DomainID
			Expect(nbClient.ReverseProxyDomains.Delete(ctx, oldDomainID)).To(Succeed())
			_, err = reconcileOnce(r, "gate")
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rpc), rpc)).To(Succeed())
			Expect(rpc.Status.DomainID).NotTo(BeEmpty())
			Expect(rpc.Status.DomainID).NotTo(Equal(oldDomainID)) // freshly recreated
			domains, err := nbClient.ReverseProxyDomains.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(domains).To(HaveLen(1))
			Expect(domains[0].Domain).To(Equal("ccbash.cloud"))
		})

		// readyRPC creates a ReverseProxyCluster and reconciles it through the LB
		// IP + enrollment seeding to its terminal state for the current NetBird
		// fixture (Ready, or a dependency condition the caller asserts on).
		readyRPC := func(r *ReverseProxyClusterReconciler, name, domain, address string) *nbv1alpha1.ReverseProxyCluster {
			rpc := &nbv1alpha1.ReverseProxyCluster{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec:       nbv1alpha1.ReverseProxyClusterSpec{ClusterAddress: address, Domain: domain},
			}
			Expect(k8sClient.Create(ctx, rpc)).To(Succeed())
			_, err := reconcileOnce(r, name)
			Expect(err).NotTo(HaveOccurred())
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "reverseproxycluster-" + name, Namespace: ns}, svc)).To(Succeed())
			svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.60"}}
			Expect(k8sClient.Status().Update(ctx, svc)).To(Succeed())
			controls.AddProxyCluster("c-"+name, address)
			_, err = reconcileOnce(r, name)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rpc), rpc)).To(Succeed())
			return rpc
		}

		It("resets connectivity status when the proxy disappears", func() {
			r := &ReverseProxyClusterReconciler{Client: k8sClient, Netbird: nbClient, ManagementURL: "https://mgmt.test"}
			rpc := readyRPC(r, "gate", "off.test", "gate.off.test")
			Expect(rpc.Status.Online).To(BeTrue())
			Expect(rpc.Status.ConnectedProxies).To(Equal(1))

			// The cluster deregisters / all proxies die. A fresh client sidesteps
			// the netbirdutil list cache (per client) so the loss is seen now, as
			// it would be in production after the TTL.
			controls.RemoveProxyCluster("gate.off.test")
			r = &ReverseProxyClusterReconciler{Client: k8sClient, Netbird: controls.NewClient(), ManagementURL: "https://mgmt.test"}
			_, err := reconcileOnce(r, "gate")
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rpc), rpc)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(rpc.Status.Conditions, nbv1alpha1.ReadyCondition)).To(BeFalse())
			// The printcolumns must not keep reporting the last-good connectivity.
			Expect(rpc.Status.Online).To(BeFalse())
			Expect(rpc.Status.ConnectedProxies).To(BeZero())
			// The delete key survives for reconcileDelete.
			Expect(rpc.Status.ClusterAddress).To(Equal("gate.off.test"))
		})

		It("replaces a stale custom-domain registration targeting a dead cluster", func() {
			// A registration left behind targeting a cluster no CR owns (e.g. after
			// a spec.clusterAddress change) must be replaced, not adopted as-is.
			_, err := nbClient.ReverseProxyDomains.Create(ctx, api.ReverseProxyDomainRequest{
				Domain:        "solo.test",
				TargetCluster: "gate-dead.solo.test",
			})
			Expect(err).NotTo(HaveOccurred())

			r := &ReverseProxyClusterReconciler{Client: k8sClient, Netbird: nbClient, ManagementURL: "https://mgmt.test"}
			rpc := readyRPC(r, "solo", "solo.test", "gate.solo.test")
			Expect(meta.IsStatusConditionTrue(rpc.Status.Conditions, nbv1alpha1.ReadyCondition)).To(BeTrue())

			domains, err := nbClient.ReverseProxyDomains.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(domains).To(HaveLen(1))
			Expect(domains[0].TargetCluster).To(HaveValue(Equal("gate.solo.test")))
		})

		It("shares the domain across CRs and re-points it when its target CR is deleted", func() {
			r := &ReverseProxyClusterReconciler{Client: k8sClient, Netbird: nbClient, ManagementURL: "https://mgmt.test"}
			a := readyRPC(r, "gate-a", "shared.test", "gate-a.shared.test")
			Expect(meta.IsStatusConditionTrue(a.Status.Conditions, nbv1alpha1.ReadyCondition)).To(BeTrue())

			// B fronts the same domain with its own cluster: the registration
			// legitimately targets A, so B must surface the conflict, not fight.
			b := readyRPC(r, "gate-b", "shared.test", "gate-b.shared.test")
			Expect(meta.IsStatusConditionTrue(b.Status.Conditions, nbv1alpha1.ReadyCondition)).To(BeFalse())
			ready := meta.FindStatusCondition(b.Status.Conditions, nbv1alpha1.ReadyCondition)
			Expect(ready.Message).To(ContainSubstring("gate-a.shared.test"))

			// Deleting A keeps the shared domain registration but drops A's own
			// cluster registration (nobody else uses that address).
			Expect(k8sClient.Delete(ctx, a)).To(Succeed())
			_, err := reconcileOnce(r, "gate-a")
			Expect(err).NotTo(HaveOccurred())
			domains, err := nbClient.ReverseProxyDomains.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(domains).To(HaveLen(1))
			clusters, err := nbClient.ReverseProxyClusters.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(clusters).To(HaveLen(1))
			Expect(clusters[0].Address).To(Equal("gate-b.shared.test"))

			// B's next reconcile finds the registration targeting the now-unowned,
			// no-longer-existing gate-a and replaces it with one targeting itself:
			// self-healed. (Fresh client: the per-client cluster cache still holds
			// the deleted gate-a; production converges after the 30s TTL.)
			r = &ReverseProxyClusterReconciler{Client: k8sClient, Netbird: controls.NewClient(), ManagementURL: "https://mgmt.test"}
			_, err = reconcileOnce(r, "gate-b")
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(b), b)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(b.Status.Conditions, nbv1alpha1.ReadyCondition)).To(BeTrue())
			domains, err = nbClient.ReverseProxyDomains.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(domains).To(HaveLen(1))
			Expect(domains[0].TargetCluster).To(HaveValue(Equal("gate-b.shared.test")))
		})

		It("keeps shared registrations until the last same-address CR is deleted", func() {
			r := &ReverseProxyClusterReconciler{Client: k8sClient, Netbird: nbClient, ManagementURL: "https://mgmt.test"}
			a := readyRPC(r, "dup-a", "dup.test", "gate.dup.test")
			b := readyRPC(r, "dup-b", "dup.test", "gate.dup.test")
			Expect(meta.IsStatusConditionTrue(a.Status.Conditions, nbv1alpha1.ReadyCondition)).To(BeTrue())
			Expect(meta.IsStatusConditionTrue(b.Status.Conditions, nbv1alpha1.ReadyCondition)).To(BeTrue())
			Expect(b.Status.DomainID).To(Equal(a.Status.DomainID)) // adopted, not duplicated

			Expect(k8sClient.Delete(ctx, a)).To(Succeed())
			_, err := reconcileOnce(r, "dup-a")
			Expect(err).NotTo(HaveOccurred())
			domains, err := nbClient.ReverseProxyDomains.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(domains).To(HaveLen(1)) // B still fronts the domain
			clusters, err := nbClient.ReverseProxyClusters.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(clusters).NotTo(BeEmpty()) // B still uses the address

			Expect(k8sClient.Delete(ctx, b)).To(Succeed())
			_, err = reconcileOnce(r, "dup-b")
			Expect(err).NotTo(HaveOccurred())
			domains, err = nbClient.ReverseProxyDomains.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(domains).To(BeEmpty())
			clusters, err = nbClient.ReverseProxyClusters.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(clusters).To(BeEmpty())
		})

		It("does not hijack a registration targeting an unmanaged live cluster", func() {
			// Registration created out of band, pointing at a proxy cluster that
			// exists in the account but is owned by no ReverseProxyCluster CR.
			controls.AddProxyCluster("c-ext", "gate-ext.ext.test")
			_, err := nbClient.ReverseProxyDomains.Create(ctx, api.ReverseProxyDomainRequest{
				Domain:        "ext.test",
				TargetCluster: "gate-ext.ext.test",
			})
			Expect(err).NotTo(HaveOccurred())

			r := &ReverseProxyClusterReconciler{Client: k8sClient, Netbird: nbClient, ManagementURL: "https://mgmt.test"}
			rpc := readyRPC(r, "ext", "ext.test", "gate.ext.test")
			Expect(meta.IsStatusConditionTrue(rpc.Status.Conditions, nbv1alpha1.ReadyCondition)).To(BeFalse())
			ready := meta.FindStatusCondition(rpc.Status.Conditions, nbv1alpha1.ReadyCondition)
			Expect(ready.Message).To(ContainSubstring("not managed by this operator"))

			domains, err := nbClient.ReverseProxyDomains.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(domains).To(HaveLen(1))
			Expect(domains[0].TargetCluster).To(HaveValue(Equal("gate-ext.ext.test"))) // untouched
		})

		It("protects the shared domain when the deleting CR's spec.Domain has drifted", func() {
			r := &ReverseProxyClusterReconciler{Client: k8sClient, Netbird: nbClient, ManagementURL: "https://mgmt.test"}
			a := readyRPC(r, "drift-a", "drift.test", "gate-a.drift.test")
			Expect(meta.IsStatusConditionTrue(a.Status.Conditions, nbv1alpha1.ReadyCondition)).To(BeTrue())

			// B declares the same domain but has not reconciled (no recorded ids).
			b := &nbv1alpha1.ReverseProxyCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "drift-b", Namespace: ns},
				Spec:       nbv1alpha1.ReverseProxyClusterSpec{ClusterAddress: "gate-b.drift.test", Domain: "drift.test"},
			}
			Expect(k8sClient.Create(ctx, b)).To(Succeed())

			// A's spec.Domain is renamed and A deleted before re-reconciling: the
			// guard must compare B against the registration's live domain, not A's
			// drifted spec.
			a.Spec.Domain = "renamed.test"
			Expect(k8sClient.Update(ctx, a)).To(Succeed())
			Expect(k8sClient.Delete(ctx, a)).To(Succeed())
			_, err := reconcileOnce(r, "drift-a")
			Expect(err).NotTo(HaveOccurred())

			domains, err := nbClient.ReverseProxyDomains.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(domains).To(HaveLen(1))
			Expect(domains[0].Domain).To(Equal("drift.test")) // survived for B
		})

		It("resets connectivity while waiting for the LoadBalancer IP", func() {
			r := &ReverseProxyClusterReconciler{Client: k8sClient, Netbird: nbClient, ManagementURL: "https://mgmt.test"}
			rpc := readyRPC(r, "lbwait", "lbwait.test", "gate.lbwait.test")
			Expect(rpc.Status.Online).To(BeTrue())

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "reverseproxycluster-lbwait", Namespace: ns}, svc)).To(Succeed())
			svc.Status.LoadBalancer.Ingress = nil
			Expect(k8sClient.Status().Update(ctx, svc)).To(Succeed())

			_, err := reconcileOnce(r, "lbwait")
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(rpc), rpc)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(rpc.Status.Conditions, nbv1alpha1.ReadyCondition)).To(BeFalse())
			Expect(rpc.Status.Online).To(BeFalse())
			Expect(rpc.Status.ConnectedProxies).To(BeZero())
		})
	})

	Describe("DNSZone sharing", func() {
		It("does not delete a shared zone from under the surviving CR, even after a rename", func() {
			r := NewDNSZoneReconciler(k8sClient, nbClient, nil)
			zone := func(name string) *nbv1alpha1.DNSZone {
				z := &nbv1alpha1.DNSZone{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
					Spec:       nbv1alpha1.DNSZoneSpec{Name: "internal", Domain: "internal.test", Enabled: true},
				}
				Expect(k8sClient.Create(ctx, z)).To(Succeed())
				_, err := reconcileOnce(r, name)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(z), z)).To(Succeed())
				Expect(z.Status.ZoneID).NotTo(BeEmpty())
				return z
			}

			// Both CRs declare the same zone name: B adopts A's NetBird zone.
			a, b := zone("zone-a"), zone("zone-b")
			Expect(b.Status.ZoneID).To(Equal(a.Status.ZoneID))
			sharedID := a.Status.ZoneID

			// Rename A's spec.Name (the mutable field the old guard compared).
			a.Spec.Name = "internal-renamed"
			Expect(k8sClient.Update(ctx, a)).To(Succeed())
			_, err := reconcileOnce(r, "zone-a")
			Expect(err).NotTo(HaveOccurred())

			// Deleting the renamed A must not wipe the zone B still owns — the
			// guard compares the recorded ZoneID, not the drifted spec.Name.
			Expect(k8sClient.Delete(ctx, a)).To(Succeed())
			_, err = reconcileOnce(r, "zone-a")
			Expect(err).NotTo(HaveOccurred())
			_, err = nbClient.DNSZones.GetZone(ctx, sharedID)
			Expect(err).NotTo(HaveOccurred())

			// The last CR standing removes the zone.
			Expect(k8sClient.Delete(ctx, b)).To(Succeed())
			_, err = reconcileOnce(r, "zone-b")
			Expect(err).NotTo(HaveOccurred())
			_, err = nbClient.DNSZones.GetZone(ctx, sharedID)
			Expect(err).To(HaveOccurred())
		})

		It("protects a survivor that has not adopted yet from a renamed deleter", func() {
			r := NewDNSZoneReconciler(k8sClient, nbClient, nil)
			a := &nbv1alpha1.DNSZone{
				ObjectMeta: metav1.ObjectMeta{Name: "pre-a", Namespace: ns},
				Spec:       nbv1alpha1.DNSZoneSpec{Name: "pre", Domain: "pre.test", Enabled: true},
			}
			Expect(k8sClient.Create(ctx, a)).To(Succeed())
			_, err := reconcileOnce(r, "pre-a")
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(a), a)).To(Succeed())
			zoneID := a.Status.ZoneID

			// B declares the same zone name but never reconciles (no ZoneID yet);
			// A is renamed and deleted before re-reconciling, so the live zone
			// still carries the name B will adopt by.
			b := &nbv1alpha1.DNSZone{
				ObjectMeta: metav1.ObjectMeta{Name: "pre-b", Namespace: ns},
				Spec:       nbv1alpha1.DNSZoneSpec{Name: "pre", Domain: "pre.test", Enabled: true},
			}
			Expect(k8sClient.Create(ctx, b)).To(Succeed())
			a.Spec.Name = "pre-renamed"
			Expect(k8sClient.Update(ctx, a)).To(Succeed())
			Expect(k8sClient.Delete(ctx, a)).To(Succeed())
			_, err = reconcileOnce(r, "pre-a")
			Expect(err).NotTo(HaveOccurred())

			// The zone survives (guard compared B against its live name), and B
			// adopts it rather than creating a duplicate.
			_, err = nbClient.DNSZones.GetZone(ctx, zoneID)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileOnce(r, "pre-b")
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(b), b)).To(Succeed())
			Expect(b.Status.ZoneID).To(Equal(zoneID))
		})
	})

	Describe("Gateway API translation", func() {
		It("creates a ReverseProxyCluster from a Gateway and translates HTTPRoutes onto it", func() {
			all := "All"
			// Per-Gateway params, in the Gateway's namespace, referenced via the
			// Gateway's spec.infrastructure.parametersRef.
			params := &nbv1alpha1.ReverseProxyClusterParameters{
				ObjectMeta: metav1.ObjectMeta{Name: "params-" + ns, Namespace: ns},
				Spec: nbv1alpha1.ReverseProxyClusterParametersSpec{
					Private:  true,
					LogLevel: "error",
					Groups:   []nbv1alpha1.GroupReference{{Name: &all}},
				},
			}
			Expect(k8sClient.Create(ctx, params)).To(Succeed())

			// The operator owns the GatewayClass; here it carries only our
			// controllerName (no parametersRef) and is marked Accepted.
			gc := &gwv1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{Name: "netbird-" + ns},
				Spec:       gwv1.GatewayClassSpec{ControllerName: "netbird.io/gateway-controller"},
			}
			Expect(k8sClient.Create(ctx, gc)).To(Succeed())
			_, err := reconcileOnce(&GatewayClassReconciler{Client: k8sClient}, gc.Name)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(gc), gc)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(gc.Status.Conditions, string(gwv1.GatewayClassConditionStatusAccepted))).To(BeTrue())

			// Cert Secret the TLS listener references.
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "wildcard-tls", Namespace: ns},
				Type:       corev1.SecretTypeTLS,
				Data:       map[string][]byte{"tls.crt": []byte("x"), "tls.key": []byte("y")},
			})).To(Succeed())

			gw := &gwv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
				Spec: gwv1.GatewaySpec{
					GatewayClassName: gwv1.ObjectName(gc.Name),
					Infrastructure: &gwv1.GatewayInfrastructure{
						ParametersRef: &gwv1.LocalParametersReference{
							Group: "netbird.io", Kind: "ReverseProxyClusterParameters", Name: params.Name,
						},
					},
					Listeners: []gwv1.Listener{{
						Name: "https", Protocol: gwv1.HTTPSProtocolType, Port: 443,
						Hostname: ptrTo(gwv1.Hostname("*.ccbash.cloud")),
						TLS: &gwv1.ListenerTLSConfig{
							CertificateRefs: []gwv1.SecretObjectReference{{Name: gwv1.ObjectName("wildcard-tls")}},
						},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, gw)).To(Succeed())
			_, err = reconcileOnce(&GatewayReconciler{Client: k8sClient}, "web")
			Expect(err).NotTo(HaveOccurred())

			// The Gateway created an owned ReverseProxyCluster: domain/clusterAddress/cert
			// derived from the listener; private from the params.
			rpc := &nbv1alpha1.ReverseProxyCluster{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "gateway-web", Namespace: ns}, rpc)).To(Succeed())
			Expect(rpc.Spec.Domain).To(Equal("ccbash.cloud"))
			Expect(rpc.Spec.ClusterAddress).To(Equal("gate.ccbash.cloud"))
			Expect(rpc.Spec.CertSecretName).To(Equal("wildcard-tls"))
			Expect(rpc.Spec.Private).To(BeTrue())
			Expect(rpc.Spec.LogLevel).To(Equal("error"))
			Expect(rpc.OwnerReferences).To(HaveLen(1))
			Expect(rpc.OwnerReferences[0].Name).To(Equal("web"))

			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(gw), gw)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(gw.Status.Conditions, string(gwv1.GatewayConditionAccepted))).To(BeTrue())

			route := &gwv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "infisical", Namespace: ns},
				Spec: gwv1.HTTPRouteSpec{
					CommonRouteSpec: gwv1.CommonRouteSpec{
						ParentRefs: []gwv1.ParentReference{{Name: gwv1.ObjectName("web")}},
					},
					Hostnames: []gwv1.Hostname{"secrets.ccbash.cloud"},
					Rules: []gwv1.HTTPRouteRule{{
						BackendRefs: []gwv1.HTTPBackendRef{{BackendRef: gwv1.BackendRef{
							BackendObjectReference: gwv1.BackendObjectReference{
								Name: gwv1.ObjectName("infisical"), Port: ptrTo(gwv1.PortNumber(8080)),
							},
						}}},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, route)).To(Succeed())
			_, err = reconcileOnce(&HTTPRouteReconciler{Client: k8sClient}, "infisical")
			Expect(err).NotTo(HaveOccurred())

			rps := &nbv1alpha1.ReverseProxyService{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: routeChildName("infisical", "secrets.ccbash.cloud"), Namespace: ns}, rps)).To(Succeed())
			Expect(rps.Spec.Domain).To(Equal("secrets.ccbash.cloud"))
			Expect(rps.Spec.ProxyCluster).To(Equal("gate.ccbash.cloud"))
			Expect(rps.Spec.PassHostHeader).NotTo(BeNil())
			Expect(*rps.Spec.PassHostHeader).To(BeTrue())
			Expect(rps.Spec.Backends).To(HaveLen(1))
			Expect(rps.Spec.Backends[0].ServiceRef.Name).To(Equal("infisical"))
			Expect(rps.Spec.Backends[0].Port).To(Equal(8080))

			// Route reports Accepted on our parent.
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(route), route)).To(Succeed())
			Expect(route.Status.Parents).To(HaveLen(1))
			Expect(meta.IsStatusConditionTrue(route.Status.Parents[0].Conditions, string(gwv1.RouteConditionAccepted))).To(BeTrue())

			// A hostname-less route inherits listener hostnames per Gateway API —
			// but this Gateway only has a wildcard listener, so there is nothing
			// concrete to inherit: it must fail closed with an accurate reason.
			bare := &gwv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: ns},
				Spec: gwv1.HTTPRouteSpec{
					CommonRouteSpec: gwv1.CommonRouteSpec{
						ParentRefs: []gwv1.ParentReference{{Name: gwv1.ObjectName("web")}},
					},
					Rules: []gwv1.HTTPRouteRule{{
						BackendRefs: []gwv1.HTTPBackendRef{{BackendRef: gwv1.BackendRef{
							BackendObjectReference: gwv1.BackendObjectReference{
								Name: gwv1.ObjectName("app"), Port: ptrTo(gwv1.PortNumber(8080)),
							},
						}}},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, bare)).To(Succeed())
			_, err = reconcileOnce(&HTTPRouteReconciler{Client: k8sClient}, "bare")
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(bare), bare)).To(Succeed())
			Expect(bare.Status.Parents).To(HaveLen(1))
			accepted := meta.FindStatusCondition(bare.Status.Parents[0].Conditions, string(gwv1.RouteConditionAccepted))
			Expect(accepted).NotTo(BeNil())
			Expect(accepted.Status).To(Equal(metav1.ConditionFalse))
			Expect(accepted.Reason).To(Equal(string(gwv1.RouteReasonUnsupportedValue)))
			Expect(accepted.Message).To(ContainSubstring("no hostnames"))
		})

		It("creates and self-heals the operator-owned GatewayClass", func() {
			className := "netbird-managed-" + ns
			r := &GatewayClassReconciler{Client: k8sClient, ManagedClassName: className}

			// Start seeds the class with our controllerName.
			Expect(r.Start(ctx)).To(Succeed())
			gc := &gwv1.GatewayClass{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: className}, gc)).To(Succeed())
			Expect(gc.Spec.ControllerName).To(Equal(gwv1.GatewayController("netbird.io/gateway-controller")))

			// Reconciling the live class marks it Accepted.
			_, err := reconcileOnce(r, className)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: className}, gc)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(gc.Status.Conditions, string(gwv1.GatewayClassConditionStatusAccepted))).To(BeTrue())

			// Deleting it out of band: the reconcile (fired by the delete) recreates it.
			Expect(k8sClient.Delete(ctx, gc)).To(Succeed())
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: className}, gc)).NotTo(Succeed())
			_, err = reconcileOnce(r, className)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: className}, gc)).To(Succeed())
		})
	})

	Describe("out-of-band deletion recovery", func() {
		It("Network recreates when its NetBird network was deleted out of band", func() {
			network := readyNetwork()
			oldID := network.Status.NetworkID

			// Simulate manual NetBird cleanup.
			Expect(nbClient.Networks.Delete(ctx, oldID)).To(Succeed())

			_, err := reconcileOnce(NewNetworkReconciler(k8sClient, nbClient, nil), ns)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(network), network)).To(Succeed())
			Expect(network.Status.NetworkID).NotTo(BeEmpty())
			Expect(network.Status.NetworkID).NotTo(Equal(oldID))
		})
	})
})

// ptrTo returns a pointer to v.
func ptrTo[T any](v T) *T { return &v }

// envValue returns the literal value of the named env var, or "".
func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}
