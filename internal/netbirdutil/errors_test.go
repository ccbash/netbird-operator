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
