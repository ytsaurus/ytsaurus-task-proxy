package main

import (
	"bytes"
	"context"
	"flag"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	ytsdk "go.ytsaurus.tech/yt/go/yt"

	"github.com/ytsaurus/ytsaurus-task-proxy/pkg"
)

func main() {
	ctx := context.Background()
	defaultTimeoutConfig := pkg.DefaultTaskProxyTimeoutConfig()

	var args struct {
		ytProxy                  string
		ytTokenPath              string
		baseDomain               string
		dirPath                  string
		discoveryPeriodSeconds   uint
		authEnabled              bool
		authCookieName           string
		authCacheEnabled         bool
		authCacheTTLSeconds      int
		authCacheCapacity        int
		authCacheMaxConcurrency  int
		authCacheRefreshBefore   int
		connectTimeoutSeconds    int
		routeTimeoutSeconds      int
		streamIdleTimeoutSeconds int
	}
	flag.StringVar(&args.ytProxy, "yt-proxy", "", "YT proxy host")
	flag.StringVar(&args.ytTokenPath, "yt-token-path", "", "YT token path")
	flag.StringVar(&args.baseDomain, "base-domain", "", "base domain for jobs")
	flag.StringVar(&args.dirPath, "dir-path", "", "Task proxy directory path")
	flag.UintVar(&args.discoveryPeriodSeconds, "discovery-period-seconds", 60, "services discovery period in seconds")
	flag.BoolVar(&args.authEnabled, "auth-enabled", true, "operation auth enabled")
	flag.StringVar(&args.authCookieName, "auth-cookie-name", "", "auth cookie name")
	flag.BoolVar(&args.authCacheEnabled, "auth-cache-enabled", false, "enable auth cache")
	flag.IntVar(&args.authCacheTTLSeconds, "auth-cache-ttl-seconds", 0, "auth cache entry TTL in seconds (0 means no expiration)")
	flag.IntVar(&args.authCacheCapacity, "auth-cache-capacity", 0, "auth cache maximum number of entries (0 means unlimited)")
	flag.IntVar(&args.authCacheMaxConcurrency, "auth-cache-max-concurrent-backend-requests", 0, "auth cache max concurrent backend requests per key on misses (0 means unlimited)")
	flag.IntVar(&args.authCacheRefreshBefore, "auth-cache-refresh-before-seconds", 0, "auth cache proactive refresh threshold in seconds before TTL deadline (0 disables proactive refresh)")
	flag.IntVar(&args.connectTimeoutSeconds, "connect-timeout-seconds", int(defaultTimeoutConfig.ConnectTimeout/time.Second), "maximum time in seconds to establish an upstream job connection")
	flag.IntVar(&args.routeTimeoutSeconds, "route-timeout-seconds", int(defaultTimeoutConfig.RouteTimeout/time.Second), "maximum time in seconds to wait for a complete upstream response (0 disables the timeout)")
	flag.IntVar(&args.streamIdleTimeoutSeconds, "stream-idle-timeout-seconds", int(defaultTimeoutConfig.StreamIdleTimeout/time.Second), "maximum idle time in seconds for an upstream request or response stream (0 disables the timeout)")
	flag.Parse()

	if args.ytProxy == "" {
		log.Fatal("'yt-proxy' argument is required")
	}
	if args.ytTokenPath == "" {
		log.Fatal("'yt-token-path' argument is required")
	}
	if args.baseDomain == "" {
		log.Fatal("'base-domain' argument is required")
	}
	if args.dirPath == "" {
		log.Fatal("'dir-path' argument is required")
	}
	if args.discoveryPeriodSeconds < 1 || args.discoveryPeriodSeconds > 24*60*60 {
		log.Fatal("'discovery-period-seconds' argument must be positive and not greater than 24 hours")
	}
	if args.authCacheTTLSeconds < 0 {
		log.Fatal("'auth-cache-ttl-seconds' argument must be non-negative")
	}
	if args.authCacheCapacity < 0 {
		log.Fatal("'auth-cache-capacity' argument must be non-negative")
	}
	if args.authCacheMaxConcurrency < 0 {
		log.Fatal("'auth-cache-max-concurrent-backend-requests' argument must be non-negative")
	}
	if args.authCacheRefreshBefore < 0 {
		log.Fatal("'auth-cache-refresh-before-seconds' argument must be non-negative")
	}
	connectTimeout, err := pkg.DurationFromSeconds(args.connectTimeoutSeconds)
	if err != nil {
		log.Fatalf("invalid connect timeout: %v", err)
	}
	routeTimeout, err := pkg.DurationFromSeconds(args.routeTimeoutSeconds)
	if err != nil {
		log.Fatalf("invalid route timeout: %v", err)
	}
	streamIdleTimeout, err := pkg.DurationFromSeconds(args.streamIdleTimeoutSeconds)
	if err != nil {
		log.Fatalf("invalid stream idle timeout: %v", err)
	}
	timeoutConfig := pkg.TaskProxyTimeoutConfig{
		ConnectTimeout:    connectTimeout,
		RouteTimeout:      routeTimeout,
		StreamIdleTimeout: streamIdleTimeout,
	}
	if err := timeoutConfig.Validate(); err != nil {
		log.Fatalf("invalid task proxy timeout configuration: %v", err)
	}

	ytTokenBytes, err := os.ReadFile(args.ytTokenPath)
	if err != nil {
		log.Fatalf("failed to read YT token: %v", err)
	}
	ytToken := strings.TrimSpace(string(ytTokenBytes))

	logger := pkg.SimpleLogger{}

	ytClient, err := pkg.CreateYTClient(args.ytProxy, &ytsdk.TokenCredentials{Token: ytToken}, &logger)
	if err != nil {
		pkg.DefaultMetrics().ObserveYTError("create_client", err)
		log.Fatalf("failed to create YT client: %v", err)
	}

	tls := false
	if _, err := os.Stat(pkg.TLSCrtPath); err == nil {
		if _, err := os.Stat(pkg.TLSKeyPath); err == nil {
			tls = true
		}
	}

	cache := cachev3.NewSnapshotCache(true, cachev3.IDHash{}, logger)

	taskDiscovery := pkg.CreateTaskDiscovery(args.baseDomain, args.dirPath, ytClient, &logger)

	authServer := pkg.CreateAuthServer(ytClient, args.ytProxy, &logger, args.authCookieName, pkg.AuthCacheConfig{
		Enabled:                      args.authCacheEnabled,
		TTLSeconds:                   args.authCacheTTLSeconds,
		Capacity:                     args.authCacheCapacity,
		MaxConcurrentBackendRequests: args.authCacheMaxConcurrency,
		RefreshBeforeSeconds:         args.authCacheRefreshBefore,
	})

	taskUpdater := pkg.CreateTaskUpdater(args.baseDomain, tls, args.authEnabled, timeoutConfig, authServer, taskDiscovery, cache)

	go func() {
		if err := pkg.ServeMetrics(pkg.DefaultGatherer()); err != nil {
			log.Fatalf("failed to serve metrics: %v", err)
		}
	}()

	go func() {
		var version string
		discoveryPeriod := time.Duration(args.discoveryPeriodSeconds) * time.Second
		for {
			tasks, err := taskDiscovery.Discovery(ctx)
			if err != nil {
				pkg.DefaultMetrics().ObserveDiscoveryFailure("discovery", err)
				logger.Errorf("failed to discover tasks: %v", err)
				time.Sleep(discoveryPeriod)
				continue // preserve old version of table, err is probably transient
			}

			sort.Sort(tasks)
			hashToTask := make(map[string]pkg.Task)
			operationAliasToID := make(map[string]string)
			var buf bytes.Buffer
			for _, task := range tasks {
				buf.Write([]byte(task.IDWithHostPort()))
				hashToTask[task.Hash()] = task
				if task.OperationAlias() != "" {
					operationAliasToID[task.OperationAlias()] = task.OperationID()
				}
			}

			newVersion := pkg.Hash(buf.Bytes())
			if version == newVersion {
				pkg.DefaultMetrics().ObserveDiscoverySuccess("no_changes")
				logger.Debugf("no changes in discovered tasks")
			} else {
				logger.Infof("%d tasks discovered:\n%s", len(tasks), tasks)
				version = newVersion

				err = taskUpdater.Update(ctx, hashToTask, operationAliasToID, version)
				if err != nil {
					pkg.DefaultMetrics().ObserveDiscoveryFailure("update", err)
					logger.Errorf("failed to update tasks: %v", err)
					version = "" // drop version so we will retry update on next iteration
				} else {
					pkg.DefaultMetrics().ObserveDiscoverySuccess("updated")
				}
			}

			time.Sleep(discoveryPeriod)
		}
	}()
	if err := pkg.ServeGRPC(serverv3.NewServer(ctx, cache, nil), authServer); err != nil {
		log.Fatalf("failed to serve gRPC: %v", err)
	}
}
