// SPDX-License-Identifier: BSD-3-Clause

package netbirdutil

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"
)

// GetDNSZoneByName returns the DNS zone with the given name. Selection is
// deterministic: ListZones order is not guaranteed and, should two zones ever
// share a name, the chosen zone's Id would otherwise flip between reconciles
// and churn (create/delete) the resource's DNS records. We collect all matches
// and return the one with the lowest Id so the result is stable.
func GetDNSZoneByName(ctx context.Context, nbClient *netbird.Client, name string) (api.Zone, error) {
	resp, err := nbClient.DNSZones.ListZones(ctx)
	if err != nil {
		return api.Zone{}, err
	}
	matches := make([]api.Zone, 0, 1)
	for _, zone := range resp {
		if zone.Name == name {
			matches = append(matches, zone)
		}
	}
	if len(matches) == 0 {
		return api.Zone{}, fmt.Errorf("zone with name %s cannot be found", name)
	}
	slices.SortFunc(matches, func(a, b api.Zone) int {
		return cmp.Compare(a.Id, b.Id)
	})
	return matches[0], nil
}
