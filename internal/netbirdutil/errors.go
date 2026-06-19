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
// 400 Bad Request, 409 Conflict, or 412 Precondition Failed. Callers use it to
// back off and retry (for example, deleting a group that is still attached to a
// resource, policy, router or setup key, or a network resource still targeted by
// a reverse-proxy service) instead of treating it as a hard, log-spamming
// failure.
//
// 412 is what NetBird returns for "resource is in use by proxy": deleting a
// network resource to recreate it for a routing-mode change fails until the
// reverse-proxy service releases it.
func IsConflict(err error) bool {
	var apiErr *netbird.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusBadRequest, http.StatusConflict, http.StatusPreconditionFailed:
			return true
		}
	}
	return false
}

// IsTargetTypeMismatch reports whether err is the transient error returned
// while a routing-mode switch is in flight: the reverse-proxy service was
// updated with the new target type (host/domain) but the NetworkResource still
// references the old-typed resource for a moment. It resolves once the
// resource's status ID is updated to the new-typed resource, so callers back
// off and retry instead of treating it as a hard failure.
func IsTargetTypeMismatch(err error) bool {
	var apiErr *netbird.APIError
	if errors.As(err, &apiErr) {
		return strings.Contains(apiErr.Message, "target_type") && strings.Contains(apiErr.Message, "resource is of type")
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
