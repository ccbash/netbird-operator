// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"errors"

	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/netbirdutil"
)

// +kubebuilder:rbac:groups=netbird.io,resources=dnszones,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=netbird.io,resources=dnszones/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=netbird.io,resources=dnszones/finalizers,verbs=update

// NewDNSZoneReconciler builds the mirror reconciler for the DNSZone CRD.
func NewDNSZoneReconciler(c client.Client, nb *netbird.Client, rec record.EventRecorder) *MirrorReconciler[*nbv1alpha1.DNSZone] {
	return &MirrorReconciler[*nbv1alpha1.DNSZone]{
		Client:   c,
		Netbird:  nb,
		Recorder: rec,
		m: mirror[*nbv1alpha1.DNSZone]{
			kind:      "DNSZone",
			finalizer: "dnszone",
			newObject: func() *nbv1alpha1.DNSZone { return &nbv1alpha1.DNSZone{} },
			apply:     applyDNSZone,
			del:       deleteDNSZone,
		},
	}
}

func applyDNSZone(ctx context.Context, nb *netbird.Client, c client.Client, z *nbv1alpha1.DNSZone) error {
	distGroups, err := netbirdutil.GetGroupIDs(ctx, c, nb, z.Spec.DistributionGroups, z.Namespace)
	if err != nil {
		return err
	}
	if distGroups == nil {
		distGroups = []string{}
	}

	enableSearch := false
	if z.Spec.EnableSearchDomain != nil {
		enableSearch = *z.Spec.EnableSearchDomain
	}
	enabled := z.Spec.Enabled
	req := api.ZoneRequest{
		Name:               z.Spec.Name,
		Domain:             z.Spec.Domain,
		DistributionGroups: distGroups,
		EnableSearchDomain: enableSearch,
		Enabled:            &enabled,
	}

	// Verify the recorded zone still exists; a clean 404 on GET means it was
	// deleted out of band, so drop the stale id and re-adopt/recreate below.
	if z.Status.ZoneID != "" {
		if _, err := nb.DNSZones.GetZone(ctx, z.Status.ZoneID); netbird.IsNotFound(err) {
			z.Status.ZoneID = ""
		} else if err != nil {
			return err
		}
	}

	zoneID := z.Status.ZoneID
	if zoneID == "" {
		// Adopt an existing zone with the same name rather than failing to create
		// a duplicate — a zone provisioned out of band is taken over.
		existing, err := netbirdutil.GetDNSZoneByName(ctx, nb, z.Spec.Name)
		switch {
		case err == nil:
			zoneID = existing.Id
		case errors.Is(err, netbirdutil.ErrZoneNotFound):
			// no match; create below
		default:
			return err
		}
	}

	if zoneID != "" {
		resp, err := nb.DNSZones.UpdateZone(ctx, zoneID, req)
		if err == nil {
			z.Status.ZoneID = resp.Id
			return nil
		}
		if !netbird.IsNotFound(err) {
			return err
		}
	}
	resp, err := nb.DNSZones.CreateZone(ctx, req)
	if err != nil {
		return err
	}
	z.Status.ZoneID = resp.Id
	return nil
}

func deleteDNSZone(ctx context.Context, nb *netbird.Client, z *nbv1alpha1.DNSZone) error {
	if z.Status.ZoneID == "" {
		return nil
	}
	return nb.DNSZones.DeleteZone(ctx, z.Status.ZoneID)
}
