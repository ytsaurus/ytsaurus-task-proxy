package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	ytsdk "go.ytsaurus.tech/yt/go/yt"

	"a.yandex-team.ru/cloudia/cloud/managed-yt/task-proxy/server/pkg"
)

func main() {
	ctx := context.Background()

	var args struct {
		namespace            string
		ytTokenPath          string
		baseDomain           string
		tablePath            string
		refreshPeriodSeconds uint
		authEnabled          bool
		authCookieName       string
	}
	flag.StringVar(&args.namespace, "namespace", "", "k8s namespace")
	flag.StringVar(&args.ytTokenPath, "yt-token-path", "", "YT token path")
	flag.StringVar(&args.baseDomain, "base-domain", "", "base domain for jobs")
	flag.StringVar(&args.tablePath, "table-path", "", "YT table path to store jobs info")
	flag.UintVar(&args.refreshPeriodSeconds, "refresh-period-seconds", 60, "tasks list refresh period in seconds")
	flag.BoolVar(&args.authEnabled, "auth-enabled", true, "operation auth enabled")
	flag.StringVar(&args.authCookieName, "auth-cookie-name", "yc_session", "auth cookie name")
	flag.Parse()

	if args.namespace == "" {
		log.Fatal("'namespace' argument is required")
	}
	if args.ytTokenPath == "" {
		log.Fatal("'yt-token-path' argument is required")
	}
	if args.baseDomain == "" {
		log.Fatal("'base-domain' argument is required")
	}
	if args.tablePath == "" {
		log.Fatal("'table-path' argument is required")
	}
	if args.refreshPeriodSeconds < 1 || args.refreshPeriodSeconds > 24*60*60 {
		log.Fatal("'refresh-period-seconds' argument must be positive and not greater than 24 hours")
	}

	ytTokenBytes, err := os.ReadFile(args.ytTokenPath)
	if err != nil {
		log.Fatalf("failed to read YT token: %v", err)
	}
	ytToken := strings.TrimSpace(string(ytTokenBytes))

	ytProxy := fmt.Sprintf("http-proxies-lb.%s.svc.cluster.local", args.namespace)
	ytClient, err := pkg.CreateYTClient(ytProxy, &ytsdk.TokenCredentials{Token: ytToken})
	if err != nil {
		log.Fatalf("failed to create YT client: %v", err)
	}

	tls := false
	if _, err := os.Stat(pkg.TLSCrtPath); err == nil {
		if _, err := os.Stat(pkg.TLSKeyPath); err == nil {
			tls = true
		}
	}

	logger := pkg.SimpleLogger{}

	cache := cachev3.NewSnapshotCache(true, cachev3.IDHash{}, logger)

	taskDiscovery := pkg.TaskDiscovery{
		BaseDomain: args.baseDomain,
		TablePath:  args.tablePath,
		YT:         ytClient,
		Logger:     &logger,
	}

	authServer := pkg.CreateAuthServer(ytClient, ytProxy, &logger, args.authCookieName)

	go func() {
		var version string
		for {
			tasks, err := taskDiscovery.Discovery(ctx)
			if err != nil {
				logger.Errorf("failed to discover tasks: %v", err)
				continue // preserve old version of table, err is probably transient
			}

			sort.Sort(tasks)
			hashToTask := make(map[string]pkg.Task)
			var buf bytes.Buffer
			for _, task := range tasks {
				buf.Write([]byte(task.IDWithHostPort()))
				hashToTask[pkg.Hash([]byte(task.ID()))] = task
			}

			newVersion := pkg.Hash(buf.Bytes())
			if version == newVersion {
				logger.Debugf("no changes in discovered tasks")
			} else {
				logger.Infof("%d tasks discovered:\n%s", len(tasks), tasks)
				version = newVersion

				snapshot, err := pkg.MakeSnapshot(hashToTask, version, args.baseDomain, tls, args.authEnabled)
				if err != nil {
					logger.Errorf("failed to make snapshot: %v", err)
				}

				authServer.SetHashToTasks(hashToTask)

				if err := cache.SetSnapshot(ctx, pkg.NodeID, snapshot); err != nil {
					logger.Errorf("failed to set snapshot: %v", err)
				}

				err = taskDiscovery.Save(ctx, hashToTask)
				if err != nil {
					logger.Errorf("failed to save tasks to table: %v", err)
				}
			}

			time.Sleep(time.Duration(args.refreshPeriodSeconds) * time.Second)
		}
	}()
	if err := pkg.ServeGRPC(serverv3.NewServer(ctx, cache, nil), authServer); err != nil {
		log.Fatalf("failed to serve gRPC: %v", err)
	}
}
