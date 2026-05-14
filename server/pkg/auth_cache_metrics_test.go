package pkg

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestAuthCacheMetricsHitMissAndSize(t *testing.T) {
	cache := newAuthPermissionCache(AuthCacheConfig{
		Enabled:                      true,
		TTLSeconds:                   60,
		Capacity:                     100,
		MaxConcurrentBackendRequests: 1,
	}, &SimpleLogger{})
	require.NotNil(t, cache)

	cache.metrics = NewMetrics(prometheus.NewRegistry())

	beforeHits := testutil.ToFloat64(cache.metrics.authCacheHits)
	beforeMisses := testutil.ToFloat64(cache.metrics.authCacheMisses)
	beforeSize := testutil.ToFloat64(cache.metrics.authCacheEntries)

	key := authCacheKey{credentials: "token:metrics-user", operationID: "metrics-op"}
	loadFn := func(context.Context) (bool, string, error) {
		return true, "user", nil
	}

	allowed, login, err := cache.GetOrLoad(context.Background(), key, loadFn)
	require.NoError(t, err)
	require.True(t, allowed)
	require.Equal(t, "user", login)

	allowed, login, err = cache.GetOrLoad(context.Background(), key, loadFn)
	require.NoError(t, err)
	require.True(t, allowed)
	require.Equal(t, "user", login)

	require.Equal(t, beforeHits+1, testutil.ToFloat64(cache.metrics.authCacheHits))
	require.Equal(t, beforeMisses+1, testutil.ToFloat64(cache.metrics.authCacheMisses))
	require.Equal(t, beforeSize+1, testutil.ToFloat64(cache.metrics.authCacheEntries))
}

func TestAuthCacheMetricsInFlightAndWaitingRequests(t *testing.T) {
	cache := newAuthPermissionCache(AuthCacheConfig{
		Enabled:                      true,
		TTLSeconds:                   60,
		Capacity:                     100,
		MaxConcurrentBackendRequests: 1,
	}, &SimpleLogger{})
	require.NotNil(t, cache)

	cache.metrics = NewMetrics(prometheus.NewRegistry())

	beforeInflight := testutil.ToFloat64(cache.metrics.authCacheInflightBackend)
	beforeWaiting := testutil.ToFloat64(cache.metrics.authCacheWaitingRequests)

	key := authCacheKey{credentials: "token:wait-user", operationID: "wait-op"}
	release := make(chan struct{})
	loadFn := func(context.Context) (bool, string, error) {
		<-release
		return true, "user", nil
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		allowed, login, err := cache.GetOrLoad(context.Background(), key, loadFn)
		if err == nil && (!allowed || login != "user") {
			err = errors.New("unexpected auth cache result for first request")
		}
		errs <- err
	}()

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(cache.metrics.authCacheInflightBackend) == beforeInflight+1
	}, time.Second, 10*time.Millisecond)

	go func() {
		defer wg.Done()
		allowed, login, err := cache.GetOrLoad(context.Background(), key, loadFn)
		if err == nil && (!allowed || login != "user") {
			err = errors.New("unexpected auth cache result for second request")
		}
		errs <- err
	}()

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(cache.metrics.authCacheWaitingRequests) == beforeWaiting+1
	}, time.Second, 10*time.Millisecond)

	close(release)
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(cache.metrics.authCacheInflightBackend) == beforeInflight
	}, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(cache.metrics.authCacheWaitingRequests) == beforeWaiting
	}, time.Second, 10*time.Millisecond)
}
