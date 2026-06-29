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

	readyZone := func(domain string) *nbv1alpha1.DNSZone {
		zone := &nbv1alpha1.DNSZone{
			ObjectMeta: metav1.ObjectMeta{Name: ns, Namespace: ns},
			Spec:       nbv1alpha1.DNSZoneSpec{Name: domain, Domain: domain, Enabled: true},
		}
		Expect(k8sClient.Create(ctx, zone)).To(Succeed())
		_, err := reconcileOnce(NewDNSZoneReconciler(k8sClient, nbClient, nil), ns)
		Expect(err).NotTo(HaveOccurred())
		return zone
	}

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
			readyZone("kube.example.com")
			svc := lbService("app", "192.0.2.10", "2001:db8::10")

			r := &LoadBalancerReconciler{Client: k8sClient, DefaultAdvertise: true, Network: ns, DNSZone: ns, DefaultGroups: []string{"All"}}
			_, err := reconcileOnce(r, "app")
			Expect(err).NotTo(HaveOccurred())

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
			readyZone("kube.example.com")
			nsObj := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: ns}, nsObj)).To(Succeed())
			nsObj.Annotations = map[string]string{advertiseAnnotation: "false"}
			Expect(k8sClient.Update(ctx, nsObj)).To(Succeed())
			lbService("app", "192.0.2.10")

			r := &LoadBalancerReconciler{Client: k8sClient, DefaultAdvertise: true, Network: ns, DNSZone: ns}
			_, err := reconcileOnce(r, "app")
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, client.ObjectKey{Name: "app-ipv4", Namespace: ns}, &nbv1alpha1.NetworkResource{})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("ReverseProxyService", func() {
		It("targets the backend Service's DNSRecord FQDN", func() {
			readyNetwork()
			readyZone("kube.example.com")
			lbService("app", "192.0.2.10")
			// advertise the Service so its DNSRecord exists.
			_, err := reconcileOnce(&LoadBalancerReconciler{Client: k8sClient, DefaultAdvertise: true, Network: ns, DNSZone: ns}, "app")
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
			readyZone("kube.example.com")
			// Backend with a named port (smtp) so the per-port domain reads mail-smtp.
			backend := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "mail", Namespace: ns},
				Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Ports: []corev1.ServicePort{{Name: "smtp", Port: 25}}},
			}
			Expect(k8sClient.Create(ctx, backend)).To(Succeed())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(backend), backend)).To(Succeed())
			backend.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.20"}}
			Expect(k8sClient.Status().Update(ctx, backend)).To(Succeed())
			_, err := reconcileOnce(&LoadBalancerReconciler{Client: k8sClient, DefaultAdvertise: true, Network: ns, DNSZone: ns}, "mail")
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
			readyZone("kube.example.com")
			// Multi-port mail backend with named ports.
			backend := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "mail", Namespace: ns},
				Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Ports: []corev1.ServicePort{{Name: "smtp", Port: 25}, {Name: "smtps", Port: 465}}},
			}
			Expect(k8sClient.Create(ctx, backend)).To(Succeed())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(backend), backend)).To(Succeed())
			backend.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.20"}}
			Expect(k8sClient.Status().Update(ctx, backend)).To(Succeed())
			_, err := reconcileOnce(&LoadBalancerReconciler{Client: k8sClient, DefaultAdvertise: true, Network: ns, DNSZone: ns}, "mail")
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
			readyZone("kube.example.com")

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
			_, err := reconcileOnce(&LoadBalancerReconciler{Client: k8sClient, DefaultAdvertise: true, Network: ns, DNSZone: ns}, "app")
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
			Expect(dep.Spec.Template.Spec.Containers[0].ReadinessProbe.HTTPGet.Path).To(Equal("/healthz/ready"))

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, svc)).To(Succeed())
			Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))
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
			Expect(meta.IsStatusConditionTrue(rpc.Status.Conditions, nbv1alpha1.ReadyCondition)).To(BeTrue())
		})
	})

	Describe("Gateway API translation", func() {
		It("translates an HTTPRoute on a BYOP Gateway into a ReverseProxyService", func() {
			// A ReverseProxyCluster supplies the cluster address (not reconciled here).
			rpc := &nbv1alpha1.ReverseProxyCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "gate", Namespace: ns},
				Spec:       nbv1alpha1.ReverseProxyClusterSpec{ClusterAddress: "gate.ccbash.cloud", Domain: "ccbash.cloud"},
			}
			Expect(k8sClient.Create(ctx, rpc)).To(Succeed())

			gc := &gwv1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{Name: "netbird-" + ns},
				Spec: gwv1.GatewayClassSpec{
					ControllerName: "netbird.io/byop-proxy",
					ParametersRef: &gwv1.ParametersReference{
						Group: "netbird.io", Kind: "ReverseProxyCluster",
						Name: "gate", Namespace: ptrTo(gwv1.Namespace(ns)),
					},
				},
			}
			Expect(k8sClient.Create(ctx, gc)).To(Succeed())
			_, err := reconcileOnce(&GatewayClassReconciler{Client: k8sClient}, gc.Name)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(gc), gc)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(gc.Status.Conditions, string(gwv1.GatewayClassConditionStatusAccepted))).To(BeTrue())

			gw := &gwv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
				Spec: gwv1.GatewaySpec{
					GatewayClassName: gwv1.ObjectName(gc.Name),
					Listeners:        []gwv1.Listener{{Name: "http", Protocol: gwv1.HTTPProtocolType, Port: 80}},
				},
			}
			Expect(k8sClient.Create(ctx, gw)).To(Succeed())

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
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "infisical-secrets-ccbash-cloud", Namespace: ns}, rps)).To(Succeed())
			Expect(rps.Spec.Domain).To(Equal("secrets.ccbash.cloud"))
			Expect(rps.Spec.ProxyCluster).To(Equal("gate.ccbash.cloud"))
			Expect(rps.Spec.Backends).To(HaveLen(1))
			Expect(rps.Spec.Backends[0].ServiceRef.Name).To(Equal("infisical"))
			Expect(rps.Spec.Backends[0].Port).To(Equal(8080))

			// Route reports Accepted on our parent.
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(route), route)).To(Succeed())
			Expect(route.Status.Parents).To(HaveLen(1))
			Expect(meta.IsStatusConditionTrue(route.Status.Parents[0].Conditions, string(gwv1.RouteConditionAccepted))).To(BeTrue())
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
