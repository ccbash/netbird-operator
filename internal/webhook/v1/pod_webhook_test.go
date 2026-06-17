// SPDX-License-Identifier: BSD-3-Clause

package v1

import (
	"context"
	"testing"

	"github.com/go-openapi/testify/v2/require"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
)

func TestPodInjectorSidecarProfile(t *testing.T) {
	t.Parallel()

	setupKey := &nbv1alpha1.SetupKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "test",
		},
		Spec: nbv1alpha1.SetupKeySpec{
			Name:      "test",
			Ephemeral: true,
		},
	}
	sidecarProfile := &nbv1alpha1.SidecarProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "test",
		},
		Spec: nbv1alpha1.SidecarProfileSpec{
			SetupKeyRef: corev1.LocalObjectReference{
				Name: "test",
			},
			InjectionMode: nbv1alpha1.InjectionModeContainer,
		},
	}

	scheme := kruntime.NewScheme()
	err := corev1.AddToScheme(scheme)
	require.NoError(t, err)
	err = nbv1alpha1.AddToScheme(scheme)
	require.NoError(t, err)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sidecarProfile, setupKey).Build()
	injector := PodNetbirdInjector{
		client:        k8sClient,
		managementURL: "https://api.netbird.io",
		clientImage:   "netbirdio/netbird:latest",
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "test",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{},
		},
	}
	err = injector.Default(t.Context(), pod)
	require.NoError(t, err)
	require.Len(t, pod.Spec.Containers, 1)
	require.EqualT(t, "netbird", pod.Spec.Containers[0].Name)
}

var _ = Describe("Pod Webhook", func() {
	var (
		obj       *corev1.Pod
		defaulter PodNetbirdInjector
	)

	BeforeEach(func() {
		obj = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "test",
				Namespace:   "test",
				Annotations: make(map[string]string),
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "test",
					},
				},
			},
		}
		defaulter = PodNetbirdInjector{
			client:        k8sClient,
			managementURL: "https://api.netbird.io",
			clientImage:   "netbirdio/netbird:latest",
		}
		Expect(defaulter).NotTo(BeNil(), "Expected defaulter to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")
	})

	AfterEach(func() {
	})

	Context("When creating Pod without annotation", func() {
		It("Should not modify anything", func() {
			err := defaulter.Default(context.Background(), obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(obj.Spec.Containers).To(HaveLen(1))
		})
	})
})
