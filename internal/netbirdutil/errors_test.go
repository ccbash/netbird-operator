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

	// 400, 409 and 412 are the "still in use" responses callers back off on.
	// 412 (Precondition Failed) is NetBird's "resource is in use by proxy".
	require.True(t, IsConflict(&netbird.APIError{StatusCode: http.StatusBadRequest}))
	require.True(t, IsConflict(&netbird.APIError{StatusCode: http.StatusConflict}))
	require.True(t, IsConflict(&netbird.APIError{StatusCode: http.StatusPreconditionFailed}))

	// A wrapped API error is still recognised.
	require.True(t, IsConflict(fmt.Errorf("delete group: %w", &netbird.APIError{StatusCode: http.StatusConflict})))
	require.True(t, IsConflict(fmt.Errorf("delete resource: %w", &netbird.APIError{
		StatusCode: http.StatusPreconditionFailed,
		Message:    "resource d8pdh105n19c73a0u7lg is in use by proxy d8pdhg05n19c73a0u8r0",
	})))

	// Other statuses, non-API errors and nil are not conflicts.
	require.False(t, IsConflict(&netbird.APIError{StatusCode: http.StatusNotFound}))
	require.False(t, IsConflict(&netbird.APIError{StatusCode: http.StatusInternalServerError}))
	require.False(t, IsConflict(errors.New("boom")))
	require.False(t, IsConflict(nil))
}

func TestIsTargetTypeMismatch(t *testing.T) {
	t.Parallel()

	// The transient routing-mode-switch error.
	require.True(t, IsTargetTypeMismatch(&netbird.APIError{
		StatusCode: http.StatusUnprocessableEntity,
		Message:    `target "res-1" has target_type "host" but resource is of type "domain"`,
	}))
	// Wrapped, still recognised.
	require.True(t, IsTargetTypeMismatch(fmt.Errorf("update proxy: %w", &netbird.APIError{
		Message: `target_type "domain" but resource is of type "host"`,
	})))

	// Unrelated validation errors and non-API errors are not this case.
	require.False(t, IsTargetTypeMismatch(&netbird.APIError{Message: "invalid CIDR"}))
	require.False(t, IsTargetTypeMismatch(errors.New("target_type")))
	require.False(t, IsTargetTypeMismatch(nil))
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
