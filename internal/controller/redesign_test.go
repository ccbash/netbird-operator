// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
				Annotations: map[string]string{
					networkAnnotation: ns,
					zoneAnnotation:    ns,
				},
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
	})

	Describe("LoadBalancer translation", func() {
		It("advertises a dualstack LB Service as NetworkResource + DNSRecord per family", func() {
			readyNetwork()
			readyZone("kube.example.com")
			svc := lbService("app", "192.0.2.10", "2001:db8::10")

			r := &LoadBalancerReconciler{Client: k8sClient, DefaultAdvertise: true}
			_, err := reconcileOnce(r, "app")
			Expect(err).NotTo(HaveOccurred())

			v4 := &nbv1alpha1.NetworkResource{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "app-ipv4", Namespace: ns}, v4)).To(Succeed())
			Expect(v4.Spec.Address).To(Equal("192.0.2.10"))
			Expect(v4.Spec.NetworkRef.Name).To(Equal(ns))

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

			r := &LoadBalancerReconciler{Client: k8sClient, DefaultAdvertise: true}
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
			_, err := reconcileOnce(&LoadBalancerReconciler{Client: k8sClient, DefaultAdvertise: true}, "app")
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
			Expect(target.TargetId).To(Equal("cluster-1"))
			Expect(target.Host).NotTo(BeNil())
			Expect(*target.Host).To(Equal(fmt.Sprintf("app-%s.kube.example.com", ns)))
			Expect(target.Port).To(Equal(80))
			Expect(target.Options).NotTo(BeNil())
			Expect(target.Options.DirectUpstream).NotTo(BeNil())
			Expect(*target.Options.DirectUpstream).To(BeTrue())
		})
	})
})
