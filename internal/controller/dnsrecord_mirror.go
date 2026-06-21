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

// +kubebuilder:rbac:groups=netbird.io,resources=dnsrecords,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=netbird.io,resources=dnsrecords/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=netbird.io,resources=dnsrecords/finalizers,verbs=update

// NewDNSRecordReconciler builds the mirror reconciler for the DNSRecord CRD.
func NewDNSRecordReconciler(c client.Client, nb *netbird.Client, rec record.EventRecorder) *MirrorReconciler[*nbv1alpha1.DNSRecord] {
	return &MirrorReconciler[*nbv1alpha1.DNSRecord]{
		Client:   c,
		Netbird:  nb,
		Recorder: rec,
		m: mirror[*nbv1alpha1.DNSRecord]{
			kind:      "DNSRecord",
			finalizer: "dnsrecord",
			newObject: func() *nbv1alpha1.DNSRecord { return &nbv1alpha1.DNSRecord{} },
			apply:     applyDNSRecord,
			del:       deleteDNSRecord,
		},
	}
}

func applyDNSRecord(ctx context.Context, nb *netbird.Client, c client.Client, rec *nbv1alpha1.DNSRecord) error {
	zoneID, err := resolveZoneID(ctx, c, rec.Spec.ZoneRef)
	if err != nil {
		return err
	}
	// If the zone was recreated under a new id, the recorded record id is stale.
	if rec.Status.ZoneID != "" && rec.Status.ZoneID != zoneID {
		rec.Status.RecordID = ""
	}
	rec.Status.ZoneID = zoneID

	// Verify the recorded record still exists (clean 404 on GET => recreate).
	if rec.Status.RecordID != "" {
		if _, err := nb.DNSZones.GetRecord(ctx, zoneID, rec.Status.RecordID); netbird.IsNotFound(err) {
			rec.Status.RecordID = ""
		} else if err != nil {
			return err
		}
	}

	req := api.DNSRecordRequest{
		Content: rec.Spec.Content,
		Name:    rec.Spec.Name,
		Ttl:     rec.Spec.TTL,
		Type:    api.DNSRecordType(rec.Spec.Type),
	}

	// Adopt an existing matching record (NetBird rejects identical duplicates),
	// so a status that has drifted from NetBird can't cause a duplicate create.
	if rec.Status.RecordID == "" {
		records, err := nb.DNSZones.ListRecords(ctx, zoneID)
		if err != nil {
			return err
		}
		want := recordMatchKey(rec.Spec.Type, rec.Spec.Content)
		for i := range records {
			if records[i].Name == rec.Spec.Name && recordMatchKey(string(records[i].Type), records[i].Content) == want {
				rec.Status.RecordID = records[i].Id
				break
			}
		}
	}

	if rec.Status.RecordID != "" {
		resp, err := nb.DNSZones.UpdateRecord(ctx, zoneID, rec.Status.RecordID, req)
		if err == nil {
			rec.Status.RecordID = resp.Id
			return nil
		}
		if !netbird.IsNotFound(err) {
			return err
		}
	}
	resp, err := nb.DNSZones.CreateRecord(ctx, zoneID, req)
	if err != nil {
		return err
	}
	rec.Status.RecordID = resp.Id
	return nil
}

func deleteDNSRecord(ctx context.Context, nb *netbird.Client, rec *nbv1alpha1.DNSRecord) error {
	if rec.Status.RecordID == "" || rec.Status.ZoneID == "" {
		return nil
	}
	return nb.DNSZones.DeleteRecord(ctx, rec.Status.ZoneID, rec.Status.RecordID)
}
