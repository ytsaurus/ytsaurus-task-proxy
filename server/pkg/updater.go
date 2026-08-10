package pkg

import (
	"context"
	"fmt"

	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
)

type snapshotSetter interface {
	SetSnapshot(ctx context.Context, node string, snapshot cachev3.ResourceSnapshot) error
}

type taskUpdater struct {
	baseDomain    string
	tls           bool
	authEnabled   bool
	timeoutConfig TaskProxyTimeoutConfig

	authServer    *authServer
	taskDiscovery *taskDiscovery
	cache         snapshotSetter
}

func CreateTaskUpdater(
	baseDomain string,
	tls bool,
	authEnabled bool,
	timeoutConfig TaskProxyTimeoutConfig,
	authServer *authServer,
	taskDiscovery *taskDiscovery,
	cache snapshotSetter,
) *taskUpdater {
	return &taskUpdater{
		baseDomain:    baseDomain,
		tls:           tls,
		authEnabled:   authEnabled,
		timeoutConfig: timeoutConfig,
		authServer:    authServer,
		taskDiscovery: taskDiscovery,
		cache:         cache,
	}
}

func (u *taskUpdater) Update(
	ctx context.Context,
	hashToTask map[string]Task,
	operationAliasToID map[string]string,
	version string,
) error {
	snapshot, err := makeSnapshot(hashToTask, version, u.baseDomain, u.tls, u.authEnabled, u.timeoutConfig)
	if err != nil {
		return fmt.Errorf("failed to make snapshot: %v", err)
	}

	if err := u.cache.SetSnapshot(ctx, NodeID, snapshot); err != nil {
		return fmt.Errorf("failed to set snapshot: %v", err)
	}
	u.authServer.SetTasksData(hashToTask, operationAliasToID)

	err = u.taskDiscovery.save(ctx, hashToTask)
	if err != nil {
		return fmt.Errorf("failed to save tasks to table: %v", err)
	}

	return nil
}
