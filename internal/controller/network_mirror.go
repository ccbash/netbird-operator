// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"

	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
)

// +kubebuilder:rbac:groups=netbird.io,resources=networks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=netbird.io,resources=networks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=netbird.io,resources=networks/finalizers,verbs=update

// NewNetworkReconciler builds the mirror reconciler for the Network CRD.
func NewNetworkReconciler(c client.Client, nb *netbird.Client, rec record.EventRecorder) *MirrorReconciler[*nbv1alpha1.Network] {
	return &MirrorReconciler[*nbv1alpha1.Network]{
		Client:   c,
		Netbird:  nb,
		Recorder: rec,
		m: mirror[*nbv1alpha1.Network]{
			kind:      "Network",
			finalizer: "network",
			newObject: func() *nbv1alpha1.Network { return &nbv1alpha1.Network{} },
			apply:     applyNetwork,
			del:       deleteNetwork,
		},
	}
}

func applyNetwork(ctx context.Context, nb *netbird.Client, _ client.Client, net *nbv1alpha1.Network) error {
	req := api.NetworkRequest{Name: net.Spec.Name}
	if net.Spec.Description != "" {
		req.Description = &net.Spec.Description
	}
	// Verify the recorded network still exists. A plain GET returns a clean 404
	// when it was deleted out of band (manual cleanup), which Update does not, so
	// clear the stale id and recreate rather than erroring forever.
	if net.Status.NetworkID != "" {
		if _, err := nb.Networks.Get(ctx, net.Status.NetworkID); netbird.IsNotFound(err) {
			net.Status.NetworkID = ""
		} else if err != nil {
			return err
		}
	}
	if net.Status.NetworkID != "" {
		resp, err := nb.Networks.Update(ctx, net.Status.NetworkID, req)
		if err != nil {
			return err
		}
		net.Status.NetworkID = resp.Id
		return nil
	}
	resp, err := nb.Networks.Create(ctx, req)
	if err != nil {
		return err
	}
	net.Status.NetworkID = resp.Id
	return nil
}

func deleteNetwork(ctx context.Context, nb *netbird.Client, net *nbv1alpha1.Network) error {
	if net.Status.NetworkID == "" {
		return nil
	}
	return nb.Networks.Delete(ctx, net.Status.NetworkID)
}
