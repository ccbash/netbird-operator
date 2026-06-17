// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"testing"

	"github.com/go-openapi/testify/v2/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
)

func TestCollectBackendServices(t *testing.T) {
	t.Parallel()

	svc := func(name string) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
	}
	scheme := kruntime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc("a"), svc("b")).Build()
	ctx := context.Background()

	// Duplicate names collapse to one entry each.
	got, err := collectBackendServices(ctx, c, "ns", []string{"a", "b", "a"}, false)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Contains(t, got, "a")
	require.Contains(t, got, "b")

	// A missing backend is an error unless tolerated.
	_, err = collectBackendServices(ctx, c, "ns", []string{"a", "missing"}, false)
	require.Error(t, err)

	got, err = collectBackendServices(ctx, c, "ns", []string{"a", "missing"}, true)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Contains(t, got, "a")
}

func TestResolveWorkload(t *testing.T) {
	t.Parallel()

	r := &NetworkRouterReconciler{}
	selectorLabels := map[string]string{"app.kubernetes.io/name": "networkrouter"}

	// No override: default 3 replicas, labels are just the selector labels.
	replicas, labels, annotations, err := r.resolveWorkload(&nbv1alpha1.NetworkRouter{}, corev1ac.PodTemplateSpec(), selectorLabels)
	require.NoError(t, err)
	require.Equal(t, int32(3), replicas)
	require.Equal(t, selectorLabels, labels)
	require.Empty(t, annotations)

	// Override replicas/labels/annotations; selector labels are folded in.
	five := int32(5)
	router := &nbv1alpha1.NetworkRouter{
		Spec: nbv1alpha1.NetworkRouterSpec{
			WorkloadOverride: &nbv1alpha1.WorkloadOverride{
				Replicas:    &five,
				Labels:      map[string]string{"team": "platform"},
				Annotations: map[string]string{"note": "x"},
			},
		},
	}
	replicas, labels, annotations, err = r.resolveWorkload(router, corev1ac.PodTemplateSpec(), selectorLabels)
	require.NoError(t, err)
	require.Equal(t, int32(5), replicas)
	require.Equal(t, "platform", labels["team"])
	require.Equal(t, "networkrouter", labels["app.kubernetes.io/name"])
	require.Equal(t, map[string]string{"note": "x"}, annotations)

	// A PodTemplate override is strategic-merged into the generated template.
	podTemplate := corev1ac.PodTemplateSpec().WithLabels(map[string]string{"base": "1"})
	router.Spec.WorkloadOverride.PodTemplate = &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"extra": "2"}},
	}
	_, _, _, err = r.resolveWorkload(router, podTemplate, selectorLabels)
	require.NoError(t, err)
	require.Equal(t, "1", podTemplate.Labels["base"])
	require.Equal(t, "2", podTemplate.Labels["extra"])
}
