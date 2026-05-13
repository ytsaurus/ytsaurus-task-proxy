package pkg

import (
	"context"
	"errors"
	"testing"

	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/stretchr/testify/require"
)

type failingSnapshotSetter struct {
	calls int
	err   error
}

func (s *failingSnapshotSetter) SetSnapshot(_ context.Context, _ string, _ cachev3.ResourceSnapshot) error {
	s.calls++
	return s.err
}

func TestUpdateDoesNotChangeAuthDataIfSetSnapshotFails(t *testing.T) {
	authServer := CreateAuthServer(nil, &SimpleLogger{}, "")

	oldTask := Task{
		operationID: "op-old",
		taskName:    "task",
		service:     "svc",
	}
	oldHash := oldTask.Hash()
	authServer.SetTasksData(
		map[string]Task{oldHash: oldTask},
		map[string]string{"old_alias": oldTask.operationID},
	)

	cache := &failingSnapshotSetter{err: errors.New("set snapshot failed")}
	updater := CreateTaskUpdater("example.com", false, true, authServer, &taskDiscovery{}, cache)

	newTask := Task{
		operationID: "op-new",
		taskName:    "task",
		service:     "svc",
		jobs: []HostPort{
			{host: "127.0.0.1", port: 80},
		},
	}
	newHash := newTask.Hash()
	err := updater.Update(
		context.Background(),
		map[string]Task{newHash: newTask},
		map[string]string{"new_alias": newTask.operationID},
		"v1",
	)
	require.ErrorContains(t, err, "failed to set snapshot")
	require.Equal(t, 1, cache.calls)

	resolvedOldTask, err := authServer.findTaskByRequest("", map[string]string{idRouterHeaderName: oldHash})
	require.NoError(t, err)
	require.Equal(t, oldTask.operationID, resolvedOldTask.operationID)

	resolvedNewTask, err := authServer.findTaskByRequest("", map[string]string{idRouterHeaderName: newHash})
	require.ErrorContains(t, err, "no entry for hash")
	require.Nil(t, resolvedNewTask)
}
