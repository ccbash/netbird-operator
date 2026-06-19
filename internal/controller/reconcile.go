// SPDX-License-Identifier: BSD-3-Clause

package controller

import "time"

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
	// gatewayPoll re-checks Gateway/GatewayClass/route readiness.
	gatewayPoll = 5 * time.Second
	// backendRetry retries while a backend Service is missing.
	backendRetry = 30 * time.Second
	// cleanupRetry retries a deletion blocked because the object is still
	// referenced (a group in use).
	cleanupRetry = time.Minute
)
