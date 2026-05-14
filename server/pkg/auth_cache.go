package pkg

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	ytsdk "go.ytsaurus.tech/yt/go/yt"
)

type authCacheKey struct {
	credentials string
	operationID string
}

func credentialsKey(creds ytsdk.Credentials) string {
	if creds == nil {
		return ""
	}
	switch v := creds.(type) {
	case *ytsdk.TokenCredentials:
		return "token:" + v.Token
	case *ytsdk.BearerCredentials:
		return "bearer:" + v.Token
	case *ytsdk.CookieCredentials:
		return "cookie:" + v.Cookie.Value
	default:
		return fmt.Sprintf("%T:%v", v, v)
	}
}

type authCacheEntry struct {
	allowed   bool
	expiresAt time.Time
}

type authCacheItem struct {
	key   authCacheKey
	entry authCacheEntry
}

type authCacheLoadState struct {
	inFlight int
	waitCh   chan struct{}
}

type authPermissionCache struct {
	logger  *SimpleLogger
	metrics *Metrics

	ttl                          time.Duration
	refreshBefore                time.Duration
	capacity                     int
	maxConcurrentLoadsPerKeyMiss int
	nowFn                        func() time.Time

	mx         sync.Mutex
	lru        *list.List
	entries    map[authCacheKey]*list.Element
	loadStates map[authCacheKey]*authCacheLoadState
}

func newAuthPermissionCache(cfg AuthCacheConfig, logger *SimpleLogger) *authPermissionCache {
	if !cfg.Enabled {
		return nil
	}

	cache := &authPermissionCache{
		logger:                       logger,
		metrics:                      DefaultMetrics(),
		ttl:                          time.Duration(cfg.TTLSeconds) * time.Second,
		refreshBefore:                time.Duration(cfg.RefreshBeforeSeconds) * time.Second,
		capacity:                     cfg.Capacity,
		maxConcurrentLoadsPerKeyMiss: cfg.MaxConcurrentBackendRequests,
		nowFn:                        time.Now,
		lru:                          list.New(),
		entries:                      make(map[authCacheKey]*list.Element),
		loadStates:                   make(map[authCacheKey]*authCacheLoadState),
	}
	return cache
}

func (c *authPermissionCache) GetOrLoad(
	ctx context.Context,
	key authCacheKey,
	loadFn func(context.Context) (bool, error),
) (bool, error) {
	if c == nil {
		return loadFn(ctx)
	}

	if allowed, ok, needsRefresh := c.get(key); ok {
		c.metrics.ObserveAuthCacheHit()
		c.logger.Debugf("auth cache hit: operation_id=%q allowed=%v", key.operationID, allowed)
		if needsRefresh {
			c.logger.Debugf("auth cache preventive refresh scheduled: operation_id=%q", key.operationID)
			c.triggerRefresh(key, loadFn)
		}
		return allowed, nil
	}
	c.metrics.ObserveAuthCacheMiss()
	c.logger.Debugf("auth cache miss: operation_id=%q", key.operationID)

	return c.loadOnMiss(ctx, key, loadFn)
}

func (c *authPermissionCache) get(key authCacheKey) (allowed bool, ok bool, needsRefresh bool) {
	c.lock()
	defer c.unlock()

	elem, exists := c.entries[key]
	if !exists {
		return false, false, false
	}

	item := elem.Value.(*authCacheItem)
	now := c.nowFn()
	if c.isExpired(item.entry, now) {
		c.removeElement(elem)
		return false, false, false
	}

	c.lru.MoveToFront(elem)

	if c.refreshBefore > 0 && !item.entry.expiresAt.IsZero() {
		remaining := item.entry.expiresAt.Sub(now)
		if remaining < c.refreshBefore {
			if !c.hasLoadInFlightLocked(key) {
				needsRefresh = true
			}
		}
	}

	return item.entry.allowed, true, needsRefresh
}

func (c *authPermissionCache) triggerRefresh(
	key authCacheKey,
	loadFn func(context.Context) (bool, error),
) {
	started, _ := c.tryStartLoad(key, 1)
	if !started {
		c.logger.Debugf("auth cache preventive refresh skipped: operation_id=%q in-flight request already exists", key.operationID)
		return
	}

	c.logger.Debugf("auth cache preventive refresh started: operation_id=%q", key.operationID)

	go func() {
		_, _ = c.executeLoad(context.Background(), key, loadFn)
	}()
}

func (c *authPermissionCache) loadOnMiss(
	ctx context.Context,
	key authCacheKey,
	loadFn func(context.Context) (bool, error),
) (bool, error) {
	for {
		if allowed, ok, needsRefresh := c.get(key); ok {
			if needsRefresh {
				c.triggerRefresh(key, loadFn)
			}
			return allowed, nil
		}

		started, waitCh := c.tryStartLoad(key, c.maxConcurrentLoadsPerKeyMiss)
		if started {
			return c.executeLoad(ctx, key, loadFn)
		}
		c.logger.Debugf(
			"auth cache waiting for in-flight backend request: operation_id=%q max_concurrent_per_key=%d",
			key.operationID,
			c.maxConcurrentLoadsPerKeyMiss,
		)
		c.metrics.IncAuthCacheWaitingRequests()

		select {
		case <-waitCh:
			// Some in-flight load has completed, retry from cache.
		case <-ctx.Done():
			c.metrics.DecAuthCacheWaitingRequests()
			return false, ctx.Err()
		}
		c.metrics.DecAuthCacheWaitingRequests()
	}
}

func (c *authPermissionCache) tryStartLoad(
	key authCacheKey,
	maxInFlight int,
) (bool, chan struct{}) {
	c.lock()
	defer c.unlock()

	state := c.loadStates[key]
	if state == nil {
		state = &authCacheLoadState{
			waitCh: make(chan struct{}),
		}
		c.loadStates[key] = state
	}

	if maxInFlight <= 0 || state.inFlight < maxInFlight {
		state.inFlight++
		c.metrics.IncAuthCacheInflightBackendRequests()
		return true, nil
	}

	return false, state.waitCh
}

// hasLoadInFlightLocked expects c.mx to be held by the caller.
func (c *authPermissionCache) hasLoadInFlightLocked(key authCacheKey) bool {
	state := c.loadStates[key]
	return state != nil && state.inFlight > 0
}

func (c *authPermissionCache) executeLoad(
	ctx context.Context,
	key authCacheKey,
	loadFn func(context.Context) (bool, error),
) (bool, error) {
	allowed, err := loadFn(ctx)
	if err == nil {
		c.set(key, authCacheEntry{
			allowed:   allowed,
			expiresAt: c.expiration(),
		})
	}
	c.finishLoad(key)
	return allowed, err
}

func (c *authPermissionCache) finishLoad(key authCacheKey) {
	c.lock()
	defer c.unlock()

	state := c.loadStates[key]
	if state == nil {
		return
	}
	if state.inFlight > 0 {
		state.inFlight--
		c.metrics.DecAuthCacheInflightBackendRequests()
	}
	close(state.waitCh)
	if state.inFlight == 0 {
		delete(c.loadStates, key)
		return
	}
	state.waitCh = make(chan struct{})
}

func (c *authPermissionCache) set(key authCacheKey, entry authCacheEntry) {
	c.lock()
	defer c.unlock()

	if elem, ok := c.entries[key]; ok {
		item := elem.Value.(*authCacheItem)
		item.entry = entry
		c.lru.MoveToFront(elem)
		return
	}

	elem := c.lru.PushFront(&authCacheItem{
		key:   key,
		entry: entry,
	})
	c.entries[key] = elem
	c.metrics.IncAuthCacheEntries()

	if c.capacity > 0 && c.lru.Len() > c.capacity {
		c.removeOldest()
	}
}

// removeOldest expects c.mx to be held by the caller.
func (c *authPermissionCache) removeOldest() {
	elem := c.lru.Back()
	if elem == nil {
		return
	}
	c.removeElement(elem)
}

// removeElement expects c.mx to be held by the caller.
func (c *authPermissionCache) removeElement(elem *list.Element) {
	item := elem.Value.(*authCacheItem)
	delete(c.entries, item.key)
	c.lru.Remove(elem)
	c.metrics.DecAuthCacheEntries()
}

func (c *authPermissionCache) expiration() time.Time {
	if c.ttl <= 0 {
		return time.Time{}
	}
	return c.nowFn().Add(c.ttl)
}

func (c *authPermissionCache) isExpired(entry authCacheEntry, now time.Time) bool {
	if entry.expiresAt.IsZero() {
		return false
	}
	return !now.Before(entry.expiresAt)
}

func (c *authPermissionCache) lock() {
	c.mx.Lock()
}

func (c *authPermissionCache) unlock() {
	c.mx.Unlock()
}
