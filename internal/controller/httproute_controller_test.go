// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
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

var _ = Describe("HTTPRoute Controller", func() {
	Context("When reconciling an HTTPRoute", func() {
		ctx := context.Background()

		var (
			httpRouteRec *HTTPRouteReconciler
			netRouterRec *NetworkRouterReconciler
			setupKeyRec  *SetupKeyReconciler
			groupRec     *GroupReconciler
			nbClient     *netbird.Client
			controls     *netbirdmock.Controls
			ns           string
			gwClassName  string
		)

		BeforeEach(func() {
			nbClient, controls = netbirdmock.ClientWithControls()
			httpRouteRec = &HTTPRouteReconciler{Client: k8sClient, Netbird: nbClient}
			netRouterRec = &NetworkRouterReconciler{
				Client:        k8sClient,
				Netbird:       nbClient,
				ClientImage:   "docker.io/netbirdio/netbird:latest",
				ManagementURL: "https://netbird.io",
			}
			setupKeyRec = &SetupKeyReconciler{Client: k8sClient, Netbird: nbClient}
			groupRec = &GroupReconciler{Client: k8sClient, Netbird: nbClient}

			nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "httproute-"}}
			Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
			ns = nsObj.Name

			// GatewayClass is cluster-scoped; a unique name per spec avoids clashes.
			gwc := &gwv1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "netbird-"},
				Spec:       gwv1.GatewayClassSpec{ControllerName: gwv1.GatewayController(GatewayControllerName)},
			}
			Expect(k8sClient.Create(ctx, gwc)).To(Succeed())
			gwClassName = gwc.Name
		})

		AfterEach(func() {
			_ = k8sClient.Delete(ctx, &gwv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: gwClassName}})
			nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(nsObj), nsObj); kerrors.IsNotFound(err) {
				return
			}
			Expect(k8sClient.Delete(ctx, nsObj)).To(Succeed())
		})

		It("creates a reverse-proxy service with cluster targets and DNS records", func() {
			_, err := nbClient.DNSZones.CreateZone(ctx, api.ZoneRequest{Name: "cluster.local", Domain: "cluster.local"})
			Expect(err).ToNot(HaveOccurred())
			controls.AddProxyCluster("cluster-id", "gate.test")

			// NetworkRouter reconciled to ready (network + routing peer).
			netRouter := &nbv1alpha1.NetworkRouter{
				ObjectMeta: metav1.ObjectMeta{Name: "kube", Namespace: ns},
				Spec:       nbv1alpha1.NetworkRouterSpec{DNSZoneRef: nbv1alpha1.DNSZoneReference{Name: "cluster.local"}},
			}
			Expect(k8sClient.Create(ctx, netRouter)).To(Succeed())
			routerNN := client.ObjectKey{Name: "kube", Namespace: ns}
			for range 3 {
				_, err := netRouterRec.Reconcile(ctx, reconcile.Request{NamespacedName: routerNN})
				Expect(err).NotTo(HaveOccurred())
				key := client.ObjectKey{Name: "networkrouter-kube", Namespace: ns}
				_, err = groupRec.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				_, err = setupKeyRec.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
			}

			// Gateway of the netbird class, listener selecting the router, Programmed.
			gw := &gwv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "public", Namespace: ns},
				Spec: gwv1.GatewaySpec{
					GatewayClassName: gwv1.ObjectName(gwClassName),
					Listeners: []gwv1.Listener{{
						Name:     gwv1.SectionName("kube"),
						Protocol: gwv1.ProtocolType("gateway.netbird.io/NetworkRouter"),
						Port:     gwv1.PortNumber(1),
					}},
				},
			}
			Expect(k8sClient.Create(ctx, gw)).To(Succeed())
			meta.SetStatusCondition(&gw.Status.Conditions, metav1.Condition{
				Type:               string(gwv1.GatewayConditionProgrammed),
				Status:             metav1.ConditionTrue,
				Reason:             string(gwv1.GatewayReasonProgrammed),
				ObservedGeneration: gw.Generation,
			})
			Expect(k8sClient.Status().Update(ctx, gw)).To(Succeed())

			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "app-svc", Namespace: ns},
				Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(svc), svc)).To(Succeed())

			port := gwv1.PortNumber(80)
			hr := &gwv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns},
				Spec: gwv1.HTTPRouteSpec{
					CommonRouteSpec: gwv1.CommonRouteSpec{ParentRefs: []gwv1.ParentReference{{Name: gwv1.ObjectName(gw.Name)}}},
					Hostnames:       []gwv1.Hostname{"app.example.com"},
					Rules: []gwv1.HTTPRouteRule{{
						BackendRefs: []gwv1.HTTPBackendRef{{BackendRef: gwv1.BackendRef{
							BackendObjectReference: gwv1.BackendObjectReference{Name: "app-svc", Port: &port},
						}}},
					}},
				},
			}
			Expect(k8sClient.Create(ctx, hr)).To(Succeed())

			policy := &nbv1alpha1.NBServicePolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: ns},
				Spec: nbv1alpha1.NBServicePolicySpec{
					TargetRefs:   []gwv1.LocalPolicyTargetReference{{Group: gatewayAPIGroup, Kind: httpRouteKind, Name: "app"}},
					ProxyCluster: "gate.test",
				},
			}
			Expect(k8sClient.Create(ctx, policy)).To(Succeed())

			_, err = httpRouteRec.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKey{Name: "app", Namespace: ns}})
			Expect(err).NotTo(HaveOccurred())

			// A reverse-proxy service for the hostname with a single cluster target.
			services, err := nbClient.ReverseProxyServices.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(services).To(HaveLen(1))
			svcResp := services[0]
			Expect(svcResp.Domain).To(Equal("app.example.com"))
			Expect(svcResp.Targets).To(HaveLen(1))
			target := svcResp.Targets[0]
			Expect(target.TargetType).To(Equal(api.ServiceTargetTargetTypeCluster))
			Expect(target.TargetId).To(Equal("cluster-id"))
			Expect(target.Host).NotTo(BeNil())
			Expect(*target.Host).To(Equal(fmt.Sprintf("app-svc-%s.cluster.local", ns)))
			Expect(target.Port).To(Equal(80))
			// NetBird rejects cluster targets without direct upstream enabled.
			Expect(target.Options).NotTo(BeNil())
			Expect(target.Options.DirectUpstream).NotTo(BeNil())
			Expect(*target.Options.DirectUpstream).To(BeTrue())

			// DNS A record published for the backend FQDN.
			zones, err := nbClient.DNSZones.ListZones(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(zones).NotTo(BeEmpty())
			records, err := nbClient.DNSZones.ListRecords(ctx, zones[0].Id)
			Expect(err).NotTo(HaveOccurred())
			Expect(records).NotTo(BeEmpty())
			Expect(records[0].Name).To(Equal(fmt.Sprintf("app-svc-%s.cluster.local", ns)))
			Expect(string(records[0].Type)).To(Equal("A"))
			Expect(records[0].Content).To(Equal(svc.Spec.ClusterIP))
		})
	})
})
