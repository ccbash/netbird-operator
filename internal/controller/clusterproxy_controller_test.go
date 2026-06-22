// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/version"
)

var _ = Describe("ClusterProxy Controller", func() {
	Context("When reconciling a resource", func() {
		ctx := context.Background()

		const (
			apiKey        = "test-api-key"
			managementURL = "https://mgmt.test"
			clusterName   = "kube01"
			apiServer     = "https://kubernetes.default.svc"
		)

		nn := client.ObjectKey{Name: "test-proxy", Namespace: "default"}
		childKey := client.ObjectKey{Name: "clusterproxy-" + nn.Name, Namespace: nn.Namespace}

		var cpReconciler *ClusterProxyReconciler

		// reconcile drives the ClusterProxy controller through both passes:
		// the first creates the SetupKey and bails until it has an id (the
		// SetupKey controller's job, simulated here), the second proceeds to
		// the api-key Secret and proxy Deployment.
		reconcile := func() {
			_, err := cpReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			sk := &nbv1alpha1.SetupKey{}
			Expect(k8sClient.Get(ctx, childKey, sk)).To(Succeed())
			sk.Status.SetupKeyID = "fake-setup-key-id"
			Expect(k8sClient.Status().Update(ctx, sk)).To(Succeed())

			_, err = cpReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
		}

		BeforeEach(func() {
			cpReconciler = &ClusterProxyReconciler{
				Client:        k8sClient,
				ApiKey:        apiKey,
				ManagementURL: managementURL,
			}
		})

		AfterEach(func() {
			cp := &nbv1alpha1.ClusterProxy{}
			err := k8sClient.Get(ctx, nn, cp)
			if kerrors.IsNotFound(err) {
				return
			}
			Expect(err).ToNot(HaveOccurred())
			Expect(k8sClient.Delete(ctx, cp)).To(Succeed())
		})

		It("provisions the setup key, api-key secret and proxy Deployment", func() {
			cp := &nbv1alpha1.ClusterProxy{
				ObjectMeta: metav1.ObjectMeta{Name: nn.Name, Namespace: nn.Namespace},
				Spec: nbv1alpha1.ClusterProxySpec{
					ClusterName:        clusterName,
					APIServer:          apiServer,
					ServiceAccountName: "clusterproxy-sa",
					Groups:             []nbv1alpha1.GroupReference{{Name: ptr.To("kubernetes-admin")}},
				},
			}
			Expect(k8sClient.Create(ctx, cp)).To(Succeed())
			reconcile()

			By("creating a setup key that allows the extra DNS label clients dial")
			setupKey := &nbv1alpha1.SetupKey{}
			Expect(k8sClient.Get(ctx, childKey, setupKey)).To(Succeed())
			// allowExtraDnsLabels is what lets the proxy register
			// <cluster-name>.netbird-kubeapi-proxy — the name clients put in
			// their kubeconfig. Losing it breaks the netbird-cli link.
			Expect(setupKey.Spec.AllowExtraDnsLabels).To(BeTrue())
			Expect(setupKey.Spec.Ephemeral).To(BeTrue())
			// spec.groups flow through to the key's autoGroups so the proxy
			// peer lands in the groups the access policy targets.
			Expect(setupKey.Spec.AutoGroups).To(HaveLen(1))
			Expect(setupKey.Spec.AutoGroups[0].Name).To(HaveValue(Equal("kubernetes-admin")))

			By("creating the api-key secret the proxy uses for peer->group resolution")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, childKey, secret)).To(Succeed())
			Expect(secret.Data).To(HaveKeyWithValue("api-key", []byte(apiKey)))

			By("creating the proxy Deployment with the link-critical args")
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, childKey, dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.ServiceAccountName).To(Equal("clusterproxy-sa"))
			// replicas defaults to 3 (the CRD default) when unset.
			Expect(dep.Spec.Replicas).To(HaveValue(Equal(int32(3))))
			containers := dep.Spec.Template.Spec.Containers
			Expect(containers).To(HaveLen(1))
			Expect(containers[0].Image).To(Equal(version.KubeApiProxyImage))

			args := containers[0].Args
			// management-url must be forwarded or the self-hosted setup key is
			// rejected (the v0.6.0 regression). cluster-name derives the DNS
			// label in every client kubeconfig. api-server is the upstream the
			// proxy dials. Guard all three.
			Expect(args).To(ContainElements("--management-url", managementURL))
			Expect(args).To(ContainElements("--cluster-name", clusterName))
			Expect(args).To(ContainElements("--kubernetes-api-server", apiServer))

			By("marking the ClusterProxy Ready")
			Expect(k8sClient.Get(ctx, nn, cp)).To(Succeed())
			Expect(cp.Status.ObservedGeneration).To(Equal(cp.Generation))
			Expect(meta.IsStatusConditionTrue(cp.Status.Conditions, nbv1alpha1.ReadyCondition)).To(BeTrue())
		})

		It("honors a custom replica count", func() {
			cp := &nbv1alpha1.ClusterProxy{
				ObjectMeta: metav1.ObjectMeta{Name: nn.Name, Namespace: nn.Namespace},
				Spec: nbv1alpha1.ClusterProxySpec{
					ClusterName:        clusterName,
					APIServer:          apiServer,
					ServiceAccountName: "clusterproxy-sa",
					Replicas:           ptr.To(int32(1)),
				},
			}
			Expect(k8sClient.Create(ctx, cp)).To(Succeed())
			reconcile()

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, childKey, dep)).To(Succeed())
			Expect(dep.Spec.Replicas).To(HaveValue(Equal(int32(1))))
		})
	})
})
