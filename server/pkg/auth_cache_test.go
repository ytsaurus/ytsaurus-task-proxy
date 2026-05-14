package pkg

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuthPermissionCacheSingleflightByKey(t *testing.T) {
	cache := newAuthPermissionCache(AuthCacheConfig{
		Enabled:                      true,
		TTLSeconds:                   60,
		Capacity:                     100,
		MaxConcurrentBackendRequests: 1,
	}, &SimpleLogger{})
	require.NotNil(t, cache)

	key := authCacheKey{credentials: "token:u", operationID: "op1"}

	releaseLoad := make(chan struct{})
	var loadCalls atomic.Int32
	loadFn := func(ctx context.Context) (bool, string, error) {
		loadCalls.Add(1)
		<-releaseLoad
		return true, "user", nil
	}

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	results := make(chan bool, goroutines)
	logins := make(chan string, goroutines)
	errs := make(chan error, goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			allowed, login, err := cache.GetOrLoad(context.Background(), key, loadFn)
			results <- allowed
			logins <- login
			errs <- err
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(releaseLoad)
	wg.Wait()
	close(results)
	close(logins)
	close(errs)

	require.Equal(t, int32(1), loadCalls.Load())
	for err := range errs {
		require.NoError(t, err)
	}
	for allowed := range results {
		require.True(t, allowed)
	}
	for login := range logins {
		require.Equal(t, "user", login)
	}
}

func TestAuthPermissionCacheRespectsPerKeyConcurrentMissLimit(t *testing.T) {
	cache := newAuthPermissionCache(AuthCacheConfig{
		Enabled:                      true,
		TTLSeconds:                   60,
		Capacity:                     100,
		MaxConcurrentBackendRequests: 2,
	}, &SimpleLogger{})
	require.NotNil(t, cache)

	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	var loadCalls atomic.Int32
	releaseLoad := make(chan struct{})
	loadFn := func(ctx context.Context) (bool, string, error) {
		cur := inFlight.Add(1)
		for {
			prev := maxInFlight.Load()
			if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
				break
			}
		}
		loadCalls.Add(1)
		<-releaseLoad
		inFlight.Add(-1)
		return true, "user", nil
	}

	key := authCacheKey{
		credentials: "token:u",
		operationID: "same-op",
	}

	const requests = 20
	var wg sync.WaitGroup
	wg.Add(requests)
	errs := make(chan error, requests)
	for i := 0; i < requests; i++ {
		go func() {
			defer wg.Done()
			allowed, login, err := cache.GetOrLoad(context.Background(), key, loadFn)
			if err != nil {
				errs <- err
				return
			}
			if !allowed {
				errs <- errors.New("permission should be allowed")
				return
			}
			if login != "user" {
				errs <- errors.New("login should be user")
			}
		}()
	}

	require.Eventually(t, func() bool {
		return loadCalls.Load() >= 2
	}, time.Second, 10*time.Millisecond)
	close(releaseLoad)

	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	require.GreaterOrEqual(t, loadCalls.Load(), int32(2))
	require.LessOrEqual(t, maxInFlight.Load(), int32(2))
}

func TestAuthPermissionCacheLimitIsNotGlobal(t *testing.T) {
	cache := newAuthPermissionCache(AuthCacheConfig{
		Enabled:                      true,
		TTLSeconds:                   60,
		Capacity:                     100,
		MaxConcurrentBackendRequests: 1,
	}, &SimpleLogger{})
	require.NotNil(t, cache)

	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	releaseLoad := make(chan struct{})
	loadFn := func(ctx context.Context) (bool, string, error) {
		cur := inFlight.Add(1)
		for {
			prev := maxInFlight.Load()
			if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
				break
			}
		}
		<-releaseLoad
		inFlight.Add(-1)
		return true, "user", nil
	}

	key1 := authCacheKey{credentials: "token:u1", operationID: "op1"}
	key2 := authCacheKey{credentials: "token:u2", operationID: "op2"}

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	go func() {
		defer wg.Done()
		allowed, login, err := cache.GetOrLoad(context.Background(), key1, loadFn)
		if err == nil && (!allowed || login != "user") {
			err = errors.New("unexpected result for key1")
		}
		errs <- err
	}()
	go func() {
		defer wg.Done()
		allowed, login, err := cache.GetOrLoad(context.Background(), key2, loadFn)
		if err == nil && (!allowed || login != "user") {
			err = errors.New("unexpected result for key2")
		}
		errs <- err
	}()

	require.Eventually(t, func() bool {
		return maxInFlight.Load() >= 2
	}, time.Second, 10*time.Millisecond)
	close(releaseLoad)

	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.GreaterOrEqual(t, maxInFlight.Load(), int32(2))
}

func TestAuthPermissionCacheProactiveRefresh(t *testing.T) {
	cache := newAuthPermissionCache(AuthCacheConfig{
		Enabled:                      true,
		TTLSeconds:                   60,
		Capacity:                     100,
		MaxConcurrentBackendRequests: 1,
		RefreshBeforeSeconds:         30,
	}, &SimpleLogger{})
	require.NotNil(t, cache)

	// Keep the test fast and deterministic.
	cache.ttl = 100 * time.Millisecond
	cache.refreshBefore = 80 * time.Millisecond

	key := authCacheKey{credentials: "token:u", operationID: "op-proactive"}

	var loadCalls atomic.Int32
	loadFn := func(ctx context.Context) (bool, string, error) {
		call := loadCalls.Add(1)
		// first load: true, proactive refresh load: false
		if call == 1 {
			return true, "user-first", nil
		}
		return false, "user-second", nil
	}

	allowed, login, err := cache.GetOrLoad(context.Background(), key, loadFn)
	require.NoError(t, err)
	require.True(t, allowed)
	require.Equal(t, "user-first", login)
	require.Equal(t, int32(1), loadCalls.Load())

	time.Sleep(35 * time.Millisecond) // remaining TTL is now below refresh threshold.

	allowed, login, err = cache.GetOrLoad(context.Background(), key, loadFn)
	require.NoError(t, err)
	require.True(t, allowed) // stale value while refresh is happening
	require.Equal(t, "user-first", login)

	require.Eventually(t, func() bool {
		return loadCalls.Load() >= 2
	}, time.Second, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		allowed, login, err = cache.GetOrLoad(context.Background(), key, loadFn)
		require.NoError(t, err)
		return !allowed && login == "user-second"
	}, time.Second, 10*time.Millisecond)
}
