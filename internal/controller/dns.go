// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"time"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"
)

// dnsRecordTTL is the TTL applied to the A/AAAA records the operator publishes.
const dnsRecordTTL = 5 * time.Minute

// serviceFQDN builds the single-label name under the zone for a Service:
// "<svc>-<ns>.<zoneDomain>". NetBird's managed zones only serve one label below
// the apex, so the service and namespace are hyphen-joined rather than nested.
func serviceFQDN(svcName, svcNamespace, zoneDomain string) string {
	return svcName + "-" + svcNamespace + "." + zoneDomain
}

// reconcileZoneRecords makes the zone's records at fqdn match exactly one A per
// IPv4 and one AAAA per IPv6 address in addrs. It works statelessly from the
// live zone — adopting records that already match (name+type+content), creating
// the missing ones, and deleting any other record at fqdn — so it can be driven
// by either the NetworkResource (TCP/L4) or the HTTPRoute (reverse-proxy) path
// without relying on per-object status. It returns the records now present at
// fqdn.
func reconcileZoneRecords(ctx context.Context, nb *netbird.Client, zoneID, fqdn string, addrs []string) ([]api.DNSRecord, error) {
	type desiredRecord struct {
		rType   api.DNSRecordType
		content string
	}
	var desired []desiredRecord
	for _, ip := range addrs {
		rType, ok := dnsRecordTypeFor(ip)
		if !ok {
			continue
		}
		desired = append(desired, desiredRecord{rType, ip})
	}

	// Index the zone's live records at fqdn so existing ones are adopted rather
	// than recreated (avoids "identical record already exists").
	zoneRecords, err := nb.DNSZones.ListRecords(ctx, zoneID)
	if err != nil {
		return nil, err
	}
	existing := map[string]api.DNSRecord{}
	var ours []api.DNSRecord
	for _, rec := range zoneRecords {
		if rec.Name != fqdn {
			continue
		}
		ours = append(ours, rec)
		existing[recordMatchKey(string(rec.Type), rec.Content)] = rec
	}

	kept := make([]api.DNSRecord, 0, len(desired))
	desiredKeys := map[string]bool{}
	for _, d := range desired {
		key := recordMatchKey(string(d.rType), d.content)
		desiredKeys[key] = true
		if cur, ok := existing[key]; ok {
			kept = append(kept, cur)
			continue
		}
		resp, err := nb.DNSZones.CreateRecord(ctx, zoneID, api.DNSRecordRequest{
			Content: d.content,
			Name:    fqdn,
			Ttl:     int(dnsRecordTTL / time.Second),
			Type:    d.rType,
		})
		if err != nil {
			return nil, err
		}
		kept = append(kept, *resp)
	}

	// Delete stale records at this fqdn (e.g. a previous ClusterIP).
	for _, rec := range ours {
		if !desiredKeys[recordMatchKey(string(rec.Type), rec.Content)] {
			if err := nb.DNSZones.DeleteRecord(ctx, zoneID, rec.Id); err != nil && !netbird.IsNotFound(err) {
				return nil, err
			}
		}
	}
	return kept, nil
}

// deleteZoneRecords removes every record at fqdn in the zone — used on teardown
// of an exposure that has no per-object status enumerating its records (the
// reverse-proxy path).
func deleteZoneRecords(ctx context.Context, nb *netbird.Client, zoneID, fqdn string) error {
	zoneRecords, err := nb.DNSZones.ListRecords(ctx, zoneID)
	if err != nil {
		return err
	}
	for _, rec := range zoneRecords {
		if rec.Name != fqdn {
			continue
		}
		if err := nb.DNSZones.DeleteRecord(ctx, zoneID, rec.Id); err != nil && !netbird.IsNotFound(err) {
			return err
		}
	}
	return nil
}
