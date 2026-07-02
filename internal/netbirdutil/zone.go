// SPDX-License-Identifier: BSD-3-Clause

package netbirdutil

import (
	"context"
	"errors"
	"fmt"
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
	zone, found, err := cache.lookup(time.Now(), fetch, func(z api.Zone) bool { return z.Name == name })
	if err != nil {
		return api.Zone{}, err
	}
	if !found {
		return api.Zone{}, fmt.Errorf("%w: %s", ErrZoneNotFound, name)
	}
	return zone, nil
}
