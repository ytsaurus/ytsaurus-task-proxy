package pkg

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	"go.ytsaurus.tech/yt/go/ypath"
	ytsdk "go.ytsaurus.tech/yt/go/yt"
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
	updater := CreateTaskUpdater("example.com", false, true, DefaultTaskProxyTimeoutConfig(), authServer, &taskDiscovery{}, cache)

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

type discoveryTableYT struct {
	*discoveryYT
	t    *testing.T
	rows []TaskRow
}

func (c *discoveryTableYT) NodeExists(_ context.Context, path ypath.YPath, _ *ytsdk.NodeExistsOptions) (bool, error) {
	require.Equal(c.t, ypath.Path("//proxy/services"), path)
	return true, nil
}
func (c *discoveryTableYT) WriteTable(_ context.Context, path ypath.YPath, _ *ytsdk.WriteTableOptions) (ytsdk.TableWriter, error) {
	require.Equal(c.t, ypath.Path("//proxy/services"), path)
	return &discoveryTableWriter{client: c}, nil
}

type discoveryTableWriter struct {
	client *discoveryTableYT
	rows   []TaskRow
}

func (w *discoveryTableWriter) Write(value any) error {
	w.rows = append(w.rows, *value.(*TaskRow))
	return nil
}
func (w *discoveryTableWriter) Commit() error   { w.client.rows = w.rows; return nil }
func (w *discoveryTableWriter) Rollback() error { return nil }

func TestDiscoveryRetainedAndFreshTasksReachAllUpdaterOutputs(t *testing.T) {
	c := &discoveryTableYT{discoveryYT: newDiscoveryYT(), t: t}
	failing, healthy := discoveryOperation(1, "old"), discoveryOperation(2, "healthy")
	job1, job2 := discoveryJob(1, "old"), discoveryJob(2, "healthy")
	c.operations = []ytsdk.OperationStatus{failing, healthy}
	c.jobs[failing.ID], c.jobs[healthy.ID] = []ytsdk.JobStatus{job1}, []ytsdk.JobStatus{job2}
	c.ports[discoveryPortsPath(job1)], c.ports[discoveryPortsPath(job2)] = []int{8000, 8001}, []int{9000, 9001}
	d := CreateTaskDiscovery("example.com", "//proxy", c, &SimpleLogger{})
	discoverTasks(t, d, append(expectedDiscoveryTasks("0-0-0-1", "old", HostPort{"old", 8000}), expectedDiscoveryTasks("0-0-0-2", "healthy", HostPort{"healthy", 9000})...))
	c.jobErrors[failing.ID] = errors.New("YT unavailable")
	c.operations[0] = discoveryOperation(1, "changed")
	c.ports[discoveryPortsPath(job2)] = []int{10000, 10001}
	tasks := discoverTasks(t, d, append(expectedDiscoveryTasks("0-0-0-1", "old", HostPort{"old", 8000}), expectedDiscoveryTasks("0-0-0-2", "healthy", HostPort{"healthy", 10000})...))
	hashToTask := make(map[string]Task)
	aliases := make(map[string]string)
	for _, task := range tasks {
		hashToTask[task.Hash()] = task
		aliases[task.OperationAlias()] = task.OperationID()
	}
	auth := CreateAuthServer(nil, "", &SimpleLogger{}, "", AuthCacheConfig{})
	cache := cachev3.NewSnapshotCache(true, cachev3.IDHash{}, &SimpleLogger{})
	u := CreateTaskUpdater("example.com", false, true, DefaultTaskProxyTimeoutConfig(), auth, d, cache)
	require.NoError(t, u.Update(context.Background(), hashToTask, aliases, "merged"))

	// Hashes below are fixed SHA256 suffixes of the fixture IDs, independent of output.
	for _, tt := range []struct {
		hash, alias, service, id string
		protocol                 Protocol
		host                     string
		port                     uint32
	}{
		{"da464c6e", "old", "api", "0-0-0-1", GRPC, "old", 8000},
		{"3a7fd2e3", "old", "ui", "0-0-0-1", WEBSOCKET, "old", 8001},
		{"6b8f3497", "healthy", "api", "0-0-0-2", GRPC, "healthy", 10000},
		{"9f7f65e3", "healthy", "ui", "0-0-0-2", WEBSOCKET, "healthy", 10001},
	} {
		t.Run(tt.id+"/"+tt.service, func(t *testing.T) {
			for _, request := range []struct {
				host    string
				headers map[string]string
			}{
				{host: tt.hash + ".example.com"},
				{host: tt.alias + "-worker-" + tt.service + ".example.com"},
				{headers: map[string]string{idRouterHeaderName: tt.hash}},
			} {
				task, err := auth.findTaskByRequest(request.host, request.headers)
				require.NoError(t, err)
				require.Equal(t, tt.id, task.OperationID())
				require.Equal(t, tt.protocol, task.protocol)
				require.Equal(t, []HostPort{{host: tt.host, port: tt.port}}, task.jobs)
			}
		})
	}
	_, err := auth.findTaskByRequest("changed-worker-api.example.com", nil)
	require.Error(t, err)
	require.ElementsMatch(t, []TaskRow{
		{OperationID: "0-0-0-1", TaskName: "worker", Service: "api", Protocol: "grpc", Domain: "da464c6e.example.com"},
		{OperationID: "0-0-0-1", TaskName: "worker", Service: "ui", Protocol: "websocket", Domain: "3a7fd2e3.example.com"},
		{OperationID: "0-0-0-2", TaskName: "worker", Service: "api", Protocol: "grpc", Domain: "6b8f3497.example.com"},
		{OperationID: "0-0-0-2", TaskName: "worker", Service: "ui", Protocol: "websocket", Domain: "9f7f65e3.example.com"},
	}, c.rows)

	snapshot, err := cache.GetSnapshot(NodeID)
	require.NoError(t, err)
	clusters := snapshot.GetResources(resourcev3.ClusterType)
	require.Len(t, clusters, 5) // Four service clusters plus ext_authz.
	for _, tt := range []struct {
		name, host string
		port       uint32
	}{
		{"0-0-0-1-worker-api-0", "old", 8000}, {"0-0-0-1-worker-ui-0", "old", 8001},
		{"0-0-0-2-worker-api-0", "healthy", 10000}, {"0-0-0-2-worker-ui-0", "healthy", 10001},
	} {
		cluster := clusters[tt.name].(*clusterv3.Cluster)
		address := cluster.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
		require.Equal(t, tt.host, address.GetAddress())
		require.Equal(t, tt.port, address.GetPortValue())
	}
	hcm := httpConnectionManager(t, onlyListener(t, snapshot.GetResources(resourcev3.ListenerType)))
	domains := make(map[string]bool)
	routedClusters := make(map[string]int)
	for _, vhost := range hcm.GetRouteConfig().GetVirtualHosts() {
		for _, domain := range vhost.Domains {
			domains[domain] = true
		}
		for _, route := range vhost.Routes {
			action := route.GetRoute()
			if action == nil {
				continue
			}
			require.Equal(t, 600*time.Second, action.GetTimeout().AsDuration())
			require.Equal(t, 120*time.Second, action.GetIdleTimeout().AsDuration())
			name := action.GetWeightedClusters().GetClusters()[0].GetName()
			routedClusters[name]++
			if strings.Contains(name, "-ui-") {
				require.Len(t, action.UpgradeConfigs, 1)
				require.Equal(t, "websocket", action.UpgradeConfigs[0].UpgradeType)
			}
		}
	}
	for _, domain := range []string{"da464c6e.example.com", "3a7fd2e3.example.com", "6b8f3497.example.com", "9f7f65e3.example.com", "old-worker-api.example.com", "old-worker-ui.example.com", "healthy-worker-api.example.com", "healthy-worker-ui.example.com"} {
		require.True(t, domains[domain], domain)
	}
	require.False(t, domains["changed-worker-api.example.com"])
	require.Equal(t, map[string]int{"0-0-0-1-worker-api-0": 4, "0-0-0-1-worker-ui-0": 4, "0-0-0-2-worker-api-0": 4, "0-0-0-2-worker-ui-0": 4}, routedClusters)
}
