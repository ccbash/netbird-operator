// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"testing"

	"github.com/go-openapi/testify/v2/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/netbirdio/netbird/shared/management/http/api"
)

func TestResourceName(t *testing.T) {
	t.Parallel()
	// Names are suffixed with the type so host and domain resources for the same
	// Service can coexist during a create-before-delete routing-mode switch.
	require.Equal(t, "uid-1234-host", resourceName(types.UID("uid-1234"), api.NetworkResourceTypeHost))
	require.Equal(t, "uid-1234-domain", resourceName(types.UID("uid-1234"), api.NetworkResourceTypeDomain))
	require.NotEqual(t,
		resourceName(types.UID("x"), api.NetworkResourceTypeHost),
		resourceName(types.UID("x"), api.NetworkResourceTypeDomain))
}

func TestAppendUnique(t *testing.T) {
	t.Parallel()
	require.Equal(t, []string{"a"}, appendUnique(nil, "a"))
	require.Equal(t, []string{"a", "b"}, appendUnique([]string{"a"}, "b"))
	require.Equal(t, []string{"a"}, appendUnique([]string{"a"}, "a")) // dup ignored
	require.Equal(t, []string{"a", "b"}, appendUnique([]string{"a", "b"}, "a"))
}
