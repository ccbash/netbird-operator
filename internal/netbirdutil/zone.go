// SPDX-License-Identifier: BSD-3-Clause

package netbirdutil

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"
)

// ErrZoneNotFound is returned by GetDNSZoneByName when no zone matches. It is a
// transient condition while a NetworkRouter is still creating its DNS zone, so
// callers can treat it as not-ready/requeue rather than a hard error.
var ErrZoneNotFound = errors.New("dns zone not found")

func GetDNSZoneByName(ctx context.Context, nbClient *netbird.Client, name string) (api.Zone, error) {
	cache := &cachesFor(nbClient).zones
	fetch := func() ([]api.Zone, error) { return nbClient.DNSZones.ListZones(ctx) }
	find := func(items []api.Zone) (api.Zone, bool) {
		i := slices.IndexFunc(items, func(z api.Zone) bool { return z.Name == name })
		if i == -1 {
			return api.Zone{}, false
		}
		return items[i], true
	}

	now := time.Now()
	zones, err := cache.list(now, fetch)
	if err != nil {
		return api.Zone{}, err
	}
	if z, ok := find(zones); ok {
		return z, nil
	}
	// Miss — refetch in case it was just created before reporting not-found.
	zones, err = cache.refresh(now, fetch)
	if err != nil {
		return api.Zone{}, err
	}
	if z, ok := find(zones); ok {
		return z, nil
	}
	return api.Zone{}, fmt.Errorf("%w: %s", ErrZoneNotFound, name)
}
