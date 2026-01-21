package pkg

import (
	"context"
	"fmt"

	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
)

type taskUpdater struct {
	baseDomain  string
	tls         bool
	authEnabled bool

	authServer    *authServer
	taskDiscovery *taskDiscovery
	cache         cachev3.SnapshotCache
}

func CreateTaskUpdater(
	baseDomain string,
	tls bool,
	authEnabled bool,
	authServer *authServer,
	taskDiscovery *taskDiscovery,
	cache cachev3.SnapshotCache,
) *taskUpdater {
	return &taskUpdater{
		baseDomain:    baseDomain,
		tls:           tls,
		authEnabled:   authEnabled,
		authServer:    authServer,
		taskDiscovery: taskDiscovery,
		cache:         cache,
	}
}

func (u *taskUpdater) Update(ctx context.Context, hashToTask map[string]Task, version string) error {
	snapshot, err := makeSnapshot(hashToTask, version, u.baseDomain, u.tls, u.authEnabled)
	if err != nil {
		return fmt.Errorf("failed to make snapshot: %v", err)
	}

	u.authServer.SetHashToTasks(hashToTask)

	if err := u.cache.SetSnapshot(ctx, NodeID, snapshot); err != nil {
		return fmt.Errorf("failed to set snapshot: %v", err)
	}

	err = u.taskDiscovery.save(ctx, hashToTask)
	if err != nil {
		return fmt.Errorf("failed to save tasks to table: %v", err)
	}

	return nil
}
