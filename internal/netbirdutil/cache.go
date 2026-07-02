// SPDX-License-Identifier: BSD-3-Clause

package netbirdutil

import (
	"slices"
	"sync"
	"time"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"
)

// listCacheTTL bounds how long a cached NetBird list is reused before a refetch.
// Short enough that out-of-band changes are picked up quickly, long enough that
// a burst of reconciles (and the periodic resync) shares one API call.
const listCacheTTL = 30 * time.Second

// listCache memoizes a slice fetched from the NetBird API for listCacheTTL.
type listCache[T any] struct {
	mu      sync.Mutex
	items   []T
	ok      bool
	expires time.Time
}

// list returns the cached slice if still fresh, otherwise fetches and caches it.
func (c *listCache[T]) list(now time.Time, fetch func() ([]T, error)) ([]T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && now.Before(c.expires) {
		return c.items, nil
	}
	return c.fetchLocked(now, fetch)
}

// refresh always fetches and caches, bypassing the freshness check — used to
// confirm a miss isn't just staleness before reporting not-found.
func (c *listCache[T]) refresh(now time.Time, fetch func() ([]T, error)) ([]T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fetchLocked(now, fetch)
}

func (c *listCache[T]) fetchLocked(now time.Time, fetch func() ([]T, error)) ([]T, error) {
	items, err := fetch()
	if err != nil {
		return nil, err
	}
	c.items = items
	c.ok = true
	c.expires = now.Add(listCacheTTL)
	return items, nil
}

// lookup returns the first cached item matching match, refetching once on a miss
// before reporting not-found — so a just-created item isn't missed due to a
// stale cache. found is false (nil error) when nothing matches after the
// refetch; a non-nil error is a fetch failure.
func (c *listCache[T]) lookup(now time.Time, fetch func() ([]T, error), match func(T) bool) (item T, found bool, err error) {
	items, err := c.list(now, fetch)
	if err != nil {
		return item, false, err
	}
	if i := slices.IndexFunc(items, match); i != -1 {
		return items[i], true, nil
	}
	items, err = c.refresh(now, fetch)
	if err != nil {
		return item, false, err
	}
	if i := slices.IndexFunc(items, match); i != -1 {
		return items[i], true, nil
	}
	return item, false, nil
}

// apiCaches holds the per-client list caches for the "list everything to find
// one" lookups (clusters, zones).
type apiCaches struct {
	clusters listCache[api.ProxyCluster]
	zones    listCache[api.Zone]
}

// caches maps a NetBird client to its caches. The operator uses one client; in
// tests each gets its own entry, so there is no cross-test contamination.
var caches sync.Map // *netbird.Client -> *apiCaches

func cachesFor(c *netbird.Client) *apiCaches {
	v, _ := caches.LoadOrStore(c, &apiCaches{})
	return v.(*apiCaches)
}
