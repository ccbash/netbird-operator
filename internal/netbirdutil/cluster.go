// SPDX-License-Identifier: BSD-3-Clause

package netbirdutil

import (
	"context"
	"errors"
	"fmt"
	"slices"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"
)

// ErrProxyClusterNotFound is returned by GetProxyClusterByAddress when no
// reverse-proxy cluster matches the requested address. The cluster is
// provisioned out of band (BYOP / shared), so a missing one is a not-ready
// dependency the caller should surface and retry, not a hard failure.
var ErrProxyClusterNotFound = errors.New("reverse-proxy cluster not found")

// GetProxyClusterByAddress resolves a NetBird reverse-proxy cluster by its
// address (e.g. "gate.ccbash.de"), which is what users configure and what the
// HTTP reverse-proxy targets reference by ID. Returns ErrProxyClusterNotFound
// when none match.
func GetProxyClusterByAddress(ctx context.Context, nbClient *netbird.Client, address string) (api.ProxyCluster, error) {
	clusters, err := nbClient.ReverseProxyClusters.List(ctx)
	if err != nil {
		return api.ProxyCluster{}, err
	}
	idx := slices.IndexFunc(clusters, func(c api.ProxyCluster) bool {
		return c.Address == address
	})
	if idx == -1 {
		return api.ProxyCluster{}, fmt.Errorf("%w: %s", ErrProxyClusterNotFound, address)
	}
	return clusters[idx], nil
}
