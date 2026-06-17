// SPDX-License-Identifier: BSD-3-Clause

package netbirdutil

import (
	"errors"
	"net/http"
	"strings"

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

// IsTargetNotFound reports whether err is a NetBird API 422 (Unprocessable
// Entity) indicating a referenced target no longer exists — e.g. a reverse-proxy
// service pointing at a network resource that was deleted out of band. It is a
// transient condition while the backing resource is (re)created, so callers
// back off and retry rather than treating it as a hard failure.
func IsTargetNotFound(err error) bool {
	var apiErr *netbird.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusUnprocessableEntity && strings.Contains(apiErr.Message, "not found")
	}
	return false
}
