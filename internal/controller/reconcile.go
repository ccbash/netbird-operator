// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Requeue intervals shared across the controllers, named so the retry policy is
// tunable in one place rather than scattered as magic durations.
const (
	// resyncInterval re-reconciles healthy objects so changes made out of band
	// on the NetBird control plane are detected without waiting for the cache's
	// (multi-hour) resync.
	resyncInterval = 15 * time.Minute
	// dependencyRetry backs off while a referenced dependency (DNS zone, router,
	// a draining stale resource) isn't ready yet.
	dependencyRetry = 10 * time.Second
	// cleanupRetry retries a deletion blocked because the object is still
	// referenced (a group in use).
	cleanupRetry = time.Minute
)

// anotherLiveMatches reports whether any object in items other than self (and
// not being deleted) satisfies match — the shared-ownership scan behind the
// delete guards: adoption implies shared ownership, so a NetBird object another
// live CR still uses must not be deleted or re-pointed.
func anotherLiveMatches[T any, PT interface {
	*T
	client.Object
}](self client.Object, items []T, match func(PT) bool) bool {
	for i := range items {
		other := PT(&items[i])
		if other.GetUID() == self.GetUID() || !other.GetDeletionTimestamp().IsZero() {
			continue
		}
		if match(other) {
			return true
		}
	}
	return false
}
