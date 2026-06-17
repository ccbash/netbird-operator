// SPDX-License-Identifier: BSD-3-Clause

package netbirdutil

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/go-openapi/testify/v2/require"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
)

func TestIsConflict(t *testing.T) {
	t.Parallel()

	// 400 and 409 are the "still in use" responses callers back off on.
	require.True(t, IsConflict(&netbird.APIError{StatusCode: http.StatusBadRequest}))
	require.True(t, IsConflict(&netbird.APIError{StatusCode: http.StatusConflict}))

	// A wrapped API error is still recognised.
	require.True(t, IsConflict(fmt.Errorf("delete group: %w", &netbird.APIError{StatusCode: http.StatusConflict})))

	// Other statuses, non-API errors and nil are not conflicts.
	require.False(t, IsConflict(&netbird.APIError{StatusCode: http.StatusNotFound}))
	require.False(t, IsConflict(&netbird.APIError{StatusCode: http.StatusInternalServerError}))
	require.False(t, IsConflict(errors.New("boom")))
	require.False(t, IsConflict(nil))
}

func TestIsTargetNotFound(t *testing.T) {
	t.Parallel()

	// 422 whose message says the target is missing — the transient case.
	require.True(t, IsTargetNotFound(&netbird.APIError{
		StatusCode: http.StatusUnprocessableEntity,
		Message:    `resource target "res-123" not found in account`,
	}))
	// Wrapped, still recognised.
	require.True(t, IsTargetNotFound(fmt.Errorf("create proxy: %w", &netbird.APIError{
		StatusCode: http.StatusUnprocessableEntity,
		Message:    "not found",
	})))

	// A 422 for some other validation reason is not a missing target.
	require.False(t, IsTargetNotFound(&netbird.APIError{
		StatusCode: http.StatusUnprocessableEntity,
		Message:    "invalid CIDR",
	}))
	// A plain 404 is handled by IsNotFound, not this.
	require.False(t, IsTargetNotFound(&netbird.APIError{StatusCode: http.StatusNotFound, Message: "not found"}))
	require.False(t, IsTargetNotFound(errors.New("not found")))
	require.False(t, IsTargetNotFound(nil))
}
