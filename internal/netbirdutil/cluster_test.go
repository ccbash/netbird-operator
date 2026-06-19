// SPDX-License-Identifier: BSD-3-Clause

package netbirdutil

import (
	"context"
	"errors"
	"testing"

	"github.com/go-openapi/testify/v2/require"

	"github.com/netbirdio/kubernetes-operator/internal/netbirdmock"
)

func TestGetProxyClusterByAddress(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	nb, controls := netbirdmock.ClientWithControls()
	controls.AddProxyCluster("c-1", "gate.ccbash.de")
	controls.AddProxyCluster("c-2", "other.example")

	// Resolves by address to the right cluster ID.
	cluster, err := GetProxyClusterByAddress(ctx, nb, "gate.ccbash.de")
	require.NoError(t, err)
	require.Equal(t, "c-1", cluster.Id)

	// A missing address is a typed not-found so callers can back off.
	_, err = GetProxyClusterByAddress(ctx, nb, "missing.example")
	require.True(t, errors.Is(err, ErrProxyClusterNotFound))
}
