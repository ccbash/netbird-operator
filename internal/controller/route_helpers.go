// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
)

// collectBackendServices fetches the Services named by a route's backendRefs,
// deduplicated by name (one NetworkResource is created per Service). When
// tolerateMissing is set, Services that no longer exist are skipped instead of
// erroring — used on delete paths where a backend may already be gone.
func collectBackendServices(ctx context.Context, c client.Client, namespace string, names []string, tolerateMissing bool) (map[string]corev1.Service, error) {
	svcIdx := map[string]corev1.Service{}
	for _, name := range names {
		var svc corev1.Service
		err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &svc)
		if tolerateMissing && kerrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		svcIdx[svc.Name] = svc
	}
	return svcIdx, nil
}

// detachNetworkResource removes owner from the Service's NetworkResource,
// deleting the resource outright when owner was its last owner. Shared by the
// HTTPRoute and TCPRoute delete paths.
func detachNetworkResource(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, svc corev1.Service) error {
	netResource := &nbv1alpha1.NetworkResource{
		ObjectMeta: metav1.ObjectMeta{Name: svc.Name, Namespace: svc.Namespace},
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(netResource), netResource); err != nil {
		return err
	}
	if err := controllerutil.RemoveOwnerReference(owner, netResource, scheme); err != nil {
		return err
	}
	if len(netResource.OwnerReferences) > 1 {
		return c.Update(ctx, netResource)
	}
	// TODO: Precondition that nothing has changed.
	return c.Delete(ctx, netResource)
}
