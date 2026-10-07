package pkg

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.ytsaurus.tech/yt/go/guid"
	"go.ytsaurus.tech/yt/go/ypath"
	"go.ytsaurus.tech/yt/go/yson"
	ytsdk "go.ytsaurus.tech/yt/go/yt"
	"go.ytsaurus.tech/yt/go/yterrors"
)

func TestParseTaskProxyAnnotation(t *testing.T) {
	for _, tt := range []struct {
		name       string
		annotation any
		expected   []taskServiceInfo
	}{
		{
			name: "full annotation",
			annotation: map[string]any{
				"enabled": true,
				"tasks_info": map[string]any{
					"example_grpc_server": map[string]any{
						"server": map[string]any{
							"protocol":   "grpc",
							"port_index": 0,
						},
					},
				},
			},
			expected: []taskServiceInfo{
				{
					task:      "example_grpc_server",
					service:   "server",
					protocol:  GRPC,
					portIndex: 0,
				},
			},
		},
		{
			name: "websocket protocol",
			annotation: map[string]any{
				"enabled": true,
				"tasks_info": map[string]any{
					"ui": map[string]any{
						"ws": map[string]any{
							"protocol":   "websocket",
							"port_index": 1,
						},
					},
				},
			},
			expected: []taskServiceInfo{
				{
					task:      "ui",
					service:   "ws",
					protocol:  WEBSOCKET,
					portIndex: 1,
				},
			},
		},
		{
			name: "minimal annotation",
			annotation: map[string]any{
				"enabled": true,
			},
			expected: []taskServiceInfo{},
		},
		{
			name: "disabled annotation (false)",
			annotation: map[string]any{
				"enabled": false,
			},
			expected: nil,
		},
		{
			name:       "disabled annotation (no attribute)",
			annotation: map[string]any{},
			expected:   nil,
		},
		{
			name:       "disabled annotation (nil)",
			annotation: nil,
			expected:   nil,
		},
		{
			name: "unknown protocol",
			annotation: map[string]any{
				"enabled": true,
				"tasks_info": map[string]any{
					"example_grpc_server": map[string]any{
						"server": map[string]any{
							"protocol":   "dns",
							"port_index": 0,
						},
					},
				},
			},
			expected: []taskServiceInfo{},
		},
		{
			name: "invalid port type",
			annotation: map[string]any{
				"enabled": true,
				"tasks_info": map[string]any{
					"example_grpc_server": map[string]any{
						"server": map[string]any{
							"protocol":   "http",
							"port_index": "0",
						},
					},
				},
			},
			expected: []taskServiceInfo{},
		},
		{
			name: "missing service attributes",
			annotation: map[string]any{
				"enabled": true,
				"tasks_info": map[string]any{
					"example_grpc_server": map[string]any{
						"server": map[string]any{
							"protocol": "http",
						},
					},
				},
			},
			expected: []taskServiceInfo{},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			taskServiceInfos, _ := parseTaskProxyAnnotation(tt.annotation)
			assert.Equal(t, tt.expected, taskServiceInfos)
		})
	}
}

func TestParseInteger(t *testing.T) {
	for _, value := range []any{int(1), int64(1), int32(1), int16(1), int8(1), uint64(1), uint32(1), uint16(1), uint8(1)} {
		parsed, ok := parseInteger(value)
		require.True(t, ok)
		require.EqualValues(t, 1, parsed)
	}

	_, ok := parseInteger(uint64(math.MaxInt64) + 1)
	require.False(t, ok)
	_, ok = parseInteger("1")
	require.False(t, ok)
}

func TestParseTaskProxyAnnotationTimeoutOverrides(t *testing.T) {
	annotation := map[string]any{
		"enabled":                     true,
		"route_timeout_seconds":       600,
		"stream_idle_timeout_seconds": 120,
	}

	_, overrides := parseTaskProxyAnnotation(annotation)

	require.Equal(t, durationPtr(10*time.Minute), overrides.routeTimeout)
	require.Equal(t, durationPtr(2*time.Minute), overrides.streamIdleTimeout)
}

func TestParseTaskProxyAnnotationRejectsInvalidTimeoutOverrides(t *testing.T) {
	annotation := map[string]any{
		"enabled":               true,
		"route_timeout_seconds": -1,
	}

	services, _ := parseTaskProxyAnnotation(annotation)

	assert.Nil(t, services)
}

// discoveryYT replaces only the network boundary; discovery and downstream consumers stay real.
type discoveryYT struct {
	ytsdk.Client
	operations     []ytsdk.OperationStatus
	jobs           map[ytsdk.OperationID][]ytsdk.JobStatus
	ports          map[ypath.Path][]int
	jobErrors      map[ytsdk.OperationID]error
	portErrors     map[ypath.Path]error
	nodes          map[ypath.Path][]string
	nodeErrors     map[ypath.Path]error
	listOperations func(context.Context, *ytsdk.ListOperationsOptions) (*ytsdk.ListOperationsResult, error)
	getNode        func(context.Context, ypath.YPath, any, *ytsdk.GetNodeOptions) error
}

func (c *discoveryYT) ListOperations(ctx context.Context, opts *ytsdk.ListOperationsOptions) (*ytsdk.ListOperationsResult, error) {
	if c.listOperations != nil {
		return c.listOperations(ctx, opts)
	}
	return &ytsdk.ListOperationsResult{Operations: c.operations}, nil
}
func (c *discoveryYT) ListJobs(_ context.Context, id ytsdk.OperationID, _ *ytsdk.ListJobsOptions) (*ytsdk.ListJobsResult, error) {
	return &ytsdk.ListJobsResult{Jobs: c.jobs[id]}, c.jobErrors[id]
}
func (c *discoveryYT) GetNode(ctx context.Context, path ypath.YPath, value any, opts *ytsdk.GetNodeOptions) error {
	if c.getNode != nil {
		return c.getNode(ctx, path, value, opts)
	}
	p := path.(ypath.Path)
	*value.(*[]int) = c.ports[p]
	return c.portErrors[p]
}
func (c *discoveryYT) ListNode(_ context.Context, path ypath.YPath, value any, _ *ytsdk.ListNodeOptions) error {
	p := path.(ypath.Path)
	*value.(*[]string) = c.nodes[p]
	return c.nodeErrors[p]
}
func newDiscoveryYT() *discoveryYT {
	return &discoveryYT{
		jobs: make(map[ytsdk.OperationID][]ytsdk.JobStatus), ports: make(map[ypath.Path][]int),
		jobErrors: make(map[ytsdk.OperationID]error), portErrors: make(map[ypath.Path]error),
		nodes: make(map[ypath.Path][]string), nodeErrors: make(map[ypath.Path]error),
	}
}
func discoveryOperation(id uint32, alias string) ytsdk.OperationStatus {
	return ytsdk.OperationStatus{
		ID: ytsdk.OperationID(guid.FromParts(id, 0, 0, 0)), State: ytsdk.StateRunning,
		StartTime: yson.Time(time.Unix(int64(1000-id), 0)),
		BriefSpec: map[string]any{"alias": "*" + alias},
		RuntimeParameters: ytsdk.OperationRuntimeParameters{Annotations: map[string]any{
			"task_proxy": map[string]any{"enabled": true, "route_timeout_seconds": 600, "stream_idle_timeout_seconds": 120,
				"tasks_info": map[string]any{"worker": map[string]any{
					"api": map[string]any{"protocol": "grpc", "port_index": 0},
					"ui":  map[string]any{"protocol": "websocket", "port_index": 1},
				}},
			},
		}},
	}
}
func discoveryJob(id uint32, host string) ytsdk.JobStatus {
	return ytsdk.JobStatus{ID: ytsdk.JobID(guid.FromParts(id, 0, 0, 0)), Address: host + ":9012", TaskName: "worker", State: "running"}
}
func discoveryPortsPath(job ytsdk.JobStatus) ypath.Path {
	return ypath.Path(fmt.Sprintf("//sys/exec_nodes/%s/orchid/exec_node/job_controller/active_jobs/%s/job_ports", job.Address, job.ID))
}
func expectedDiscoveryTasks(id, alias string, jobs ...HostPort) TaskList {
	uiJobs := append([]HostPort(nil), jobs...)
	for i := range uiJobs {
		uiJobs[i].port++
	}
	return TaskList{
		{operationID: id, operationAlias: alias, taskName: "worker", service: "api", protocol: GRPC, jobs: jobs,
			timeoutOverrides: TaskTimeoutOverrides{routeTimeout: durationPtr(600 * time.Second), streamIdleTimeout: durationPtr(120 * time.Second)}},
		{operationID: id, operationAlias: alias, taskName: "worker", service: "ui", protocol: WEBSOCKET, jobs: uiJobs,
			timeoutOverrides: TaskTimeoutOverrides{routeTimeout: durationPtr(600 * time.Second), streamIdleTimeout: durationPtr(120 * time.Second)}},
	}
}
func discoverTasks(t *testing.T, d *taskDiscovery, want TaskList) TaskList {
	t.Helper()
	got, err := d.Discovery(context.Background())
	require.NoError(t, err)
	require.ElementsMatch(t, want, got)
	return got
}

func TestDiscoveryRetainsCompleteOperationThroughYTFailures(t *testing.T) {
	c := newDiscoveryYT()
	failing, healthy := discoveryOperation(1, "old"), discoveryOperation(2, "healthy")
	c.operations = []ytsdk.OperationStatus{failing, healthy}
	job1, job2, healthyJob := discoveryJob(1, "old1"), discoveryJob(2, "old2"), discoveryJob(3, "healthy")
	c.jobs[failing.ID], c.jobs[healthy.ID] = []ytsdk.JobStatus{job1, job2}, []ytsdk.JobStatus{healthyJob}
	c.ports[discoveryPortsPath(job1)], c.ports[discoveryPortsPath(job2)] = []int{8000, 8001}, []int{9000, 9001}
	c.ports[discoveryPortsPath(healthyJob)] = []int{10000, 10001}
	d := CreateTaskDiscovery("example.com", "//proxy", c, &SimpleLogger{})
	old := expectedDiscoveryTasks("0-0-0-1", "old", HostPort{"old1", 8000}, HostPort{"old2", 9000})
	discoverTasks(t, d, append(append(TaskList{}, old...), expectedDiscoveryTasks("0-0-0-2", "healthy", HostPort{"healthy", 10000})...))

	// The failed operation now has changed alias, configuration and a partial new job set.
	c.operations[0] = discoveryOperation(1, "changed")
	c.operations[0].RuntimeParameters.Annotations["task_proxy"].(map[string]any)["route_timeout_seconds"] = 1
	new1, new2 := discoveryJob(4, "new1"), discoveryJob(5, "new2")
	c.jobs[failing.ID] = []ytsdk.JobStatus{new1, new2}
	c.ports[discoveryPortsPath(new1)], c.ports[discoveryPortsPath(new2)] = []int{11000, 11001}, []int{12000, 12001}
	readErr := errors.New("YT unavailable")
	for index, failure := range []string{"list jobs", "second job ports", "list jobs again"} {
		t.Run(failure, func(t *testing.T) {
			delete(c.portErrors, discoveryPortsPath(new2))
			delete(c.jobErrors, failing.ID)
			if failure == "second job ports" {
				c.portErrors[discoveryPortsPath(new2)] = readErr
			} else {
				c.jobErrors[failing.ID] = readErr
			}
			c.ports[discoveryPortsPath(healthyJob)][0]++
			c.ports[discoveryPortsPath(healthyJob)][1]++
			want := append(append(TaskList{}, old...), expectedDiscoveryTasks("0-0-0-2", "healthy", HostPort{"healthy", []uint32{10001, 10002, 10003}[index]})...)
			discoverTasks(t, d, want)
		})
	}
	delete(c.portErrors, discoveryPortsPath(new2))
	delete(c.jobErrors, failing.ID)
	c.operations[0] = discoveryOperation(1, "changed")
	recovered := expectedDiscoveryTasks("0-0-0-1", "changed", HostPort{"new1", 11000}, HostPort{"new2", 12000})
	discoverTasks(t, d, append(recovered, expectedDiscoveryTasks("0-0-0-2", "healthy", HostPort{"healthy", 10003})...))
}

func TestDiscoveryEvictsSuccessfulEmptyAndInvalidOperations(t *testing.T) {
	for _, transition := range []string{"empty jobs", "empty ports", "disabled", "removed annotation", "invalid annotation", "invalid timeout", "invalid alias", "operation absent"} {
		t.Run(transition, func(t *testing.T) {
			c := newDiscoveryYT()
			op, job := discoveryOperation(1, "alias"), discoveryJob(1, "host")
			c.operations = []ytsdk.OperationStatus{op}
			c.jobs[op.ID] = []ytsdk.JobStatus{job}
			c.ports[discoveryPortsPath(job)] = []int{8000, 8001}
			d := CreateTaskDiscovery("example.com", "//proxy", c, &SimpleLogger{})
			discoverTasks(t, d, expectedDiscoveryTasks("0-0-0-1", "alias", HostPort{"host", 8000}))
			switch transition {
			case "empty jobs":
				c.jobs[op.ID] = nil
			case "empty ports":
				c.ports[discoveryPortsPath(job)] = nil
			case "disabled":
				c.jobErrors[op.ID] = errors.New("YT unavailable")
				op.RuntimeParameters.Annotations["task_proxy"] = map[string]any{"enabled": false}
			case "removed annotation":
				c.jobErrors[op.ID] = errors.New("YT unavailable")
				delete(op.RuntimeParameters.Annotations, "task_proxy")
			case "invalid annotation":
				c.jobErrors[op.ID] = errors.New("YT unavailable")
				op.RuntimeParameters.Annotations["task_proxy"] = "bad"
			case "invalid timeout":
				c.jobErrors[op.ID] = errors.New("YT unavailable")
				op.RuntimeParameters.Annotations["task_proxy"] = map[string]any{"enabled": true, "route_timeout_seconds": -1}
			case "invalid alias":
				op.BriefSpec["alias"] = "*bad-alias"
			case "operation absent":
				c.operations = nil
			}
			discoverTasks(t, d, nil)
			// Restoring the operation during a read error cannot resurrect evicted tasks.
			c.operations = []ytsdk.OperationStatus{discoveryOperation(1, "alias")}
			c.jobErrors[op.ID] = errors.New("YT unavailable")
			discoverTasks(t, d, nil)
		})
	}
}

func TestDiscoveryFirstEncounterFailure(t *testing.T) {
	c := newDiscoveryYT()
	op := discoveryOperation(1, "alias")
	c.operations = []ytsdk.OperationStatus{op}
	c.jobErrors[op.ID] = errors.New("YT unavailable")
	discoverTasks(t, CreateTaskDiscovery("example.com", "//proxy", c, &SimpleLogger{}), nil)
}

func TestDiscoveryFailedOperationListingPreservesState(t *testing.T) {
	for _, failPage := range []int{1, 2} {
		t.Run(fmt.Sprintf("page %d", failPage), func(t *testing.T) {
			c := newDiscoveryYT()
			op, job := discoveryOperation(1, "alias"), discoveryJob(1, "host")
			c.operations = []ytsdk.OperationStatus{op}
			c.jobs[op.ID] = []ytsdk.JobStatus{job}
			c.ports[discoveryPortsPath(job)] = []int{8000, 8001}
			d := CreateTaskDiscovery("example.com", "//proxy", c, &SimpleLogger{})
			want := expectedDiscoveryTasks("0-0-0-1", "alias", HostPort{"host", 8000})
			discoverTasks(t, d, want)
			listingErr := errors.New("listing unavailable")
			page := 0
			c.listOperations = func(context.Context, *ytsdk.ListOperationsOptions) (*ytsdk.ListOperationsResult, error) {
				page++
				if page == failPage {
					return nil, listingErr
				}
				return &ytsdk.ListOperationsResult{Operations: []ytsdk.OperationStatus{discoveryOperation(2, "other")}, Incomplete: true}, nil
			}
			got, err := d.Discovery(context.Background())
			require.ErrorIs(t, err, listingErr)
			require.Nil(t, got)
			c.listOperations = nil
			c.jobErrors[op.ID] = errors.New("jobs unavailable")
			discoverTasks(t, d, want)
		})
	}
}

func TestDiscoveryCancellationDoesNotCommitPartialPass(t *testing.T) {
	c := newDiscoveryYT()
	first, second := discoveryOperation(1, "first"), discoveryOperation(2, "second")
	job1, job2 := discoveryJob(1, "first"), discoveryJob(2, "second")
	c.operations = []ytsdk.OperationStatus{first, second}
	c.jobs[first.ID] = []ytsdk.JobStatus{job1}
	c.jobs[second.ID] = []ytsdk.JobStatus{job2}
	c.ports[discoveryPortsPath(job1)], c.ports[discoveryPortsPath(job2)] = []int{8000, 8001}, []int{9000, 9001}
	d := CreateTaskDiscovery("example.com", "//proxy", c, &SimpleLogger{})
	want := append(expectedDiscoveryTasks("0-0-0-1", "first", HostPort{"first", 8000}), expectedDiscoveryTasks("0-0-0-2", "second", HostPort{"second", 9000})...)
	discoverTasks(t, d, want)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.getNode = func(_ context.Context, path ypath.YPath, value any, _ *ytsdk.GetNodeOptions) error {
		if path == discoveryPortsPath(job2) {
			cancel()
			return context.Canceled
		}
		*value.(*[]int) = []int{10000, 10001}
		return nil
	}
	got, err := d.Discovery(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, got)
	c.getNode = nil
	c.jobErrors[first.ID], c.jobErrors[second.ID] = errors.New("unavailable"), errors.New("unavailable")
	discoverTasks(t, d, want)
}

func TestListOperationsUsesIncompleteAndStartTime(t *testing.T) {
	c := newDiscoveryYT()
	first, second := discoveryOperation(1, "first"), discoveryOperation(2, "second")
	page := 0
	c.listOperations = func(_ context.Context, opts *ytsdk.ListOperationsOptions) (*ytsdk.ListOperationsResult, error) {
		require.Contains(t, opts.Attributes, "start_time")
		require.Equal(t, ytsdk.StateRunning, *opts.State)
		require.Equal(t, ytsdk.SortDirectionPast, *opts.CursorDirection)
		page++
		if page == 1 {
			require.Nil(t, opts.Cursor)
			return &ytsdk.ListOperationsResult{Operations: []ytsdk.OperationStatus{first}, Incomplete: true}, nil
		}
		require.Equal(t, first.StartTime, *opts.Cursor)
		return &ytsdk.ListOperationsResult{Operations: []ytsdk.OperationStatus{second}}, nil
	}
	got, err := CreateTaskDiscovery("example.com", "//proxy", c, &SimpleLogger{}).listOperations(context.Background())
	require.NoError(t, err)
	require.Equal(t, []ytsdk.OperationStatus{first, second}, got)
}

func TestListOperationsRejectsIncompletePagesWithoutProgress(t *testing.T) {
	for _, mode := range []string{"empty", "zero time", "same cursor", "newer cursor"} {
		t.Run(mode, func(t *testing.T) {
			c := newDiscoveryYT()
			first := discoveryOperation(1, "first")
			page := 0
			c.listOperations = func(context.Context, *ytsdk.ListOperationsOptions) (*ytsdk.ListOperationsResult, error) {
				page++
				require.LessOrEqual(t, page, 2, "pagination must stop without progress")
				ops := []ytsdk.OperationStatus{first}
				switch mode {
				case "empty":
					ops = nil
				case "zero time":
					ops[0].StartTime = yson.Time{}
				case "newer cursor":
					if page == 2 {
						ops[0].StartTime = yson.Time(time.Unix(1001, 0))
					}
				}
				return &ytsdk.ListOperationsResult{Operations: ops, Incomplete: true}, nil
			}
			got, err := CreateTaskDiscovery("example.com", "//proxy", c, &SimpleLogger{}).listOperations(context.Background())
			require.Error(t, err)
			require.Nil(t, got)
		})
	}
}

func TestListOperationsCompleteFullPageDoesNotReadAgain(t *testing.T) {
	c := newDiscoveryYT()
	ops := make([]ytsdk.OperationStatus, 100)
	for i := range ops {
		ops[i] = discoveryOperation(uint32(i+1), "alias")
	}
	c.listOperations = func(context.Context, *ytsdk.ListOperationsOptions) (*ytsdk.ListOperationsResult, error) {
		if ops == nil {
			t.Fatal("complete full page must finish pagination")
		}
		result := ops
		ops = nil
		return &ytsdk.ListOperationsResult{Operations: result}, nil
	}
	got, err := CreateTaskDiscovery("example.com", "//proxy", c, &SimpleLogger{}).listOperations(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 100)
}

func TestDiscoverySPYTStandaloneReadFailuresAndOptionalHistory(t *testing.T) {
	for _, failingDir := range []string{"webui", "rest", "shs"} {
		t.Run(failingDir, func(t *testing.T) {
			c := newDiscoveryYT()
			op := discoveryOperation(1, "spark")
			op.RuntimeParameters.Annotations = map[string]any{"is_spark": true, "description": map[string]any{"Spark over YT": map[string]any{"discovery_path": "//spark"}}}
			c.operations = []ytsdk.OperationStatus{op}
			c.nodes["//spark/discovery/webui"], c.nodes["//spark/discovery/rest"], c.nodes["//spark/discovery/shs"] = []string{"master:8080"}, []string{"master:9090"}, []string{"history:18080"}
			d := CreateTaskDiscovery("example.com", "//proxy", c, &SimpleLogger{})
			want := TaskList{
				{operationID: "0-0-0-1", operationAlias: "spark", taskName: "master", service: "ui", protocol: HTTP, jobs: []HostPort{{"master", 8080}}},
				{operationID: "0-0-0-1", operationAlias: "spark", taskName: "master", service: "rest", protocol: HTTP, jobs: []HostPort{{"master", 9090}}},
				{operationID: "0-0-0-1", operationAlias: "spark", taskName: "history", service: "ui", protocol: HTTP, jobs: []HostPort{{"history", 18080}}},
			}
			discoverTasks(t, d, want)
			path := ypath.Path("//spark/discovery/" + failingDir)
			c.nodeErrors[path] = errors.New("YT unavailable")
			c.nodes["//spark/discovery/webui"] = []string{"newmaster:8081"}
			if failingDir == "shs" {
				// Any history read error is ignored; other services still refresh.
				updated := append(TaskList(nil), want[:2]...)
				updated[0].jobs = []HostPort{{"newmaster", 8081}}
				discoverTasks(t, d, updated)
			} else {
				discoverTasks(t, d, want)
			}
			delete(c.nodeErrors, path)
			c.nodes["//spark/discovery/webui"] = []string{"master:8080"}
			discoverTasks(t, d, want)
			// A resolve error can be wrapped and nested, as it is in real YT failures.
			c.nodeErrors[path] = fmt.Errorf("request failed: %w", &yterrors.Error{Code: yterrors.CodeGeneric, InnerErrors: []*yterrors.Error{{Code: yterrors.CodeResolveError, Message: "no such node"}}})
			if failingDir == "shs" {
				discoverTasks(t, d, want[:2])
			} else {
				discoverTasks(t, d, want)
			}
			// Malformed discovered data is not a transient YT failure.
			delete(c.nodeErrors, path)
			c.nodes["//spark/discovery/webui"] = []string{"malformed"}
			discoverTasks(t, d, nil)
			c.nodeErrors[path] = errors.New("YT unavailable")
			discoverTasks(t, d, nil)
		})
	}
}

func TestDiscoverySPYTDirectSubmitCompatibilityAndDataErrorEviction(t *testing.T) {
	c := newDiscoveryYT()
	op := discoveryOperation(1, "spark")
	op.BriefSpec["title"] = "Spark driver for app"
	op.RuntimeParameters.Annotations = map[string]any{"description": map[string]any{"Web UI": "http://driver:4040"}}
	c.operations = []ytsdk.OperationStatus{op}
	d := CreateTaskDiscovery("example.com", "//proxy", c, &SimpleLogger{})
	discoverTasks(t, d, TaskList{{operationID: "0-0-0-1", operationAlias: "spark", taskName: "driver", service: "ui", protocol: HTTP, jobs: []HostPort{{"driver", 4040}}}})
	op.RuntimeParameters.Annotations["description"] = map[string]any{"Web UI": "http://missing-port"}
	discoverTasks(t, d, nil)
}

func TestDiscoveryYTReadErrorsPreserveCause(t *testing.T) {
	readErr := errors.New("original YT failure")
	for _, method := range []string{"list jobs", "get node", "list node"} {
		t.Run(method, func(t *testing.T) {
			c := newDiscoveryYT()
			op, job := discoveryOperation(1, "alias"), discoveryJob(1, "host")
			d := CreateTaskDiscovery("example.com", "//proxy", c, &SimpleLogger{})
			var err error
			switch method {
			case "list jobs":
				c.jobErrors[op.ID] = readErr
				_, err = d.processTaskProxyAnnotatedOperation(context.Background(), op)
			case "get node":
				c.jobs[op.ID] = []ytsdk.JobStatus{job}
				c.portErrors[discoveryPortsPath(job)] = readErr
				_, err = d.processTaskProxyAnnotatedOperation(context.Background(), op)
			case "list node":
				op.RuntimeParameters.Annotations = map[string]any{"description": map[string]any{"Spark over YT": map[string]any{"discovery_path": "//spark"}}}
				c.nodeErrors["//spark/discovery/webui"] = readErr
				_, err = d.processSPYTStandaloneClusterOperation(context.Background(), op)
			}
			require.ErrorIs(t, err, readErr)
		})
	}
}

func TestDiscoveryFirstEncounterPartialJobFailurePublishesNothing(t *testing.T) {
	c := newDiscoveryYT()
	op := discoveryOperation(1, "alias")
	first, second := discoveryJob(1, "first"), discoveryJob(2, "second")
	c.operations = []ytsdk.OperationStatus{op}
	c.jobs[op.ID] = []ytsdk.JobStatus{first, second}
	c.ports[discoveryPortsPath(first)] = []int{8000, 8001}
	c.portErrors[discoveryPortsPath(second)] = errors.New("YT unavailable")
	discoverTasks(t, CreateTaskDiscovery("example.com", "//proxy", c, &SimpleLogger{}), nil)
}

func TestDiscoveryCanceledOperationListingDoesNotEvict(t *testing.T) {
	c := newDiscoveryYT()
	op, job := discoveryOperation(1, "alias"), discoveryJob(1, "host")
	c.operations = []ytsdk.OperationStatus{op}
	c.jobs[op.ID] = []ytsdk.JobStatus{job}
	c.ports[discoveryPortsPath(job)] = []int{8000, 8001}
	d := CreateTaskDiscovery("example.com", "//proxy", c, &SimpleLogger{})
	want := expectedDiscoveryTasks("0-0-0-1", "alias", HostPort{"host", 8000})
	discoverTasks(t, d, want)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.listOperations = func(context.Context, *ytsdk.ListOperationsOptions) (*ytsdk.ListOperationsResult, error) {
		cancel()
		return &ytsdk.ListOperationsResult{}, nil
	}
	got, err := d.Discovery(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, got)
	c.listOperations = nil
	c.jobErrors[op.ID] = errors.New("YT unavailable")
	discoverTasks(t, d, want)
}
