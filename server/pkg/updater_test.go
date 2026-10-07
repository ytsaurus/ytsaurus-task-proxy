package pkg

import (
	"context"
	"errors"
	"testing"

	accesslogfile3 "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/file/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
)

type failingSnapshotSetter struct {
	calls int
	err   error
}

type capturingSnapshotSetter struct{ snapshot cachev3.ResourceSnapshot }

func (s *capturingSnapshotSetter) SetSnapshot(_ context.Context, _ string, snapshot cachev3.ResourceSnapshot) error {
	s.snapshot = snapshot
	// Stop before the unrelated YT table write; the real updater already built the xDS payload.
	return errors.New("snapshot captured")
}

func TestUpdaterPropagatesAccessLogging(t *testing.T) {
	for _, config := range []AccessLogConfig{
		{Enabled: true, WriterType: "file"},
		{Enabled: false, WriterType: "stderr"},
	} {
		t.Run(config.WriterType, func(t *testing.T) {
			cache := &capturingSnapshotSetter{}
			updater := CreateTaskUpdater("example.com", false, false, DefaultTaskProxyTimeoutConfig(), config, "/tmp/updater-logs/access.log", nil, nil, cache)
			err := updater.Update(context.Background(), nil, nil, "v1")
			require.ErrorContains(t, err, "snapshot captured")
			require.NotNil(t, cache.snapshot)
			listener := onlyListener(t, cache.snapshot.GetResources(resourcev3.ListenerType))
			if !config.Enabled {
				require.Empty(t, listener.AccessLog)
				return
			}
			require.Len(t, listener.AccessLog, 1)
			var file accesslogfile3.FileAccessLog
			require.NoError(t, listener.AccessLog[0].GetTypedConfig().UnmarshalTo(&file))
			require.Equal(t, "/tmp/updater-logs/access.log", file.Path)
		})
	}
}

func (s *failingSnapshotSetter) SetSnapshot(_ context.Context, _ string, _ cachev3.ResourceSnapshot) error {
	s.calls++
	return s.err
}

func TestUpdateDoesNotChangeAuthDataIfSetSnapshotFails(t *testing.T) {
	authServer := CreateAuthServer(nil, "", &SimpleLogger{}, "", AuthCacheConfig{})

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
	updater := CreateTaskUpdater("example.com", false, true, DefaultTaskProxyTimeoutConfig(), DefaultLoggingConfig().AccessLog, "", authServer, &taskDiscovery{}, cache)

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
