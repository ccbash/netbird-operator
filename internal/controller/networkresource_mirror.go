// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"

	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/netbirdutil"
)

// +kubebuilder:rbac:groups=netbird.io,resources=networkresources,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=netbird.io,resources=networkresources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=netbird.io,resources=networkresources/finalizers,verbs=update

// NewNetworkResourceReconciler builds the mirror reconciler for the
// NetworkResource CRD.
func NewNetworkResourceReconciler(c client.Client, nb *netbird.Client, rec record.EventRecorder) *MirrorReconciler[*nbv1alpha1.NetworkResource] {
	return &MirrorReconciler[*nbv1alpha1.NetworkResource]{
		Client:   c,
		Netbird:  nb,
		Recorder: rec,
		m: mirror[*nbv1alpha1.NetworkResource]{
			kind:      "NetworkResource",
			finalizer: "networkresource",
			newObject: func() *nbv1alpha1.NetworkResource { return &nbv1alpha1.NetworkResource{} },
			apply:     applyNetworkResource,
			del:       deleteNetworkResource,
			inUseMsg:  "NetBird resource still in use (referenced by a proxy or policy); retrying deletion",
		},
	}
}

func applyNetworkResource(ctx context.Context, nb *netbird.Client, c client.Client, nr *nbv1alpha1.NetworkResource) error {
	networkID, err := resolveNetworkID(ctx, c, nr.Spec.NetworkRef)
	if err != nil {
		return err
	}
	// If the network was recreated under a new id (out-of-band deletion), the
	// recorded resource lived in the old network — drop it and recreate.
	if nr.Status.NetworkID != "" && nr.Status.NetworkID != networkID {
		nr.Status.ResourceID = ""
	}
	nr.Status.NetworkID = networkID

	groupIDs, err := netbirdutil.GetGroupIDs(ctx, c, nb, nr.Spec.Groups, nr.Namespace)
	if err != nil {
		return err
	}

	req := api.NetworkResourceRequest{
		Address: nr.Spec.Address,
		Enabled: nr.Spec.Enabled,
		Groups:  groupIDs,
		Name:    nr.Spec.Name,
	}
	if nr.Spec.Description != "" {
		req.Description = &nr.Spec.Description
	}

	resources := nb.Networks.Resources(networkID)
	// Recreate if the recorded resource was deleted out of band.
	nr.Status.ResourceID, err = verifyNetbirdID(nr.Status.ResourceID, func(id string) error {
		_, e := resources.Get(ctx, id)
		return e
	})
	if err != nil {
		return err
	}
	if nr.Status.ResourceID != "" {
		resp, err := resources.Update(ctx, nr.Status.ResourceID, req)
		if err != nil {
			return err
		}
		nr.Status.ResourceID = resp.Id
		return nil
	}
	resp, err := resources.Create(ctx, req)
	if err != nil {
		return err
	}
	nr.Status.ResourceID = resp.Id
	return nil
}

func deleteNetworkResource(ctx context.Context, nb *netbird.Client, nr *nbv1alpha1.NetworkResource) error {
	if nr.Status.ResourceID == "" || nr.Status.NetworkID == "" {
		return nil
	}
	return nb.Networks.Resources(nr.Status.NetworkID).Delete(ctx, nr.Status.ResourceID)
}
