// SPDX-License-Identifier: BSD-3-Clause

package netbirdutil

import (
	"errors"
	"net/http"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
)

// IsConflict reports whether err is a NetBird API error indicating the request
// could not be applied because the object is still referenced / in use — a
// 400 Bad Request or 409 Conflict. Callers use it to back off and retry (for
// example, deleting a group that is still attached to a resource, policy,
// router or setup key) instead of treating it as a hard, log-spamming failure.
func IsConflict(err error) bool {
	var apiErr *netbird.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusBadRequest || apiErr.StatusCode == http.StatusConflict
	}
	return false
}
