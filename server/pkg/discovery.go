package pkg

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.ytsaurus.tech/yt/go/ypath"
	"go.ytsaurus.tech/yt/go/yson"
	ytsdk "go.ytsaurus.tech/yt/go/yt"
)

const (
	servicesTableName = "services"

	taskProxyAnnotationKey          = "task_proxy"
	taskProxyEnabledKey             = "enabled"
	taskProxyTasksInfoKey           = "tasks_info"
	taskProxyProtocolKey            = "protocol"
	taskProxyPortIndexKey           = "port_index"
	taskProxyRouteTimeoutSecondsKey = "route_timeout_seconds"
	taskProxyStreamIdleTimeoutKey   = "stream_idle_timeout_seconds"
)

type taskDiscovery struct {
	baseDomain string
	tablePath  ypath.Path
	yt         ytsdk.Client

	logger *SimpleLogger
}

func CreateTaskDiscovery(baseDomain string, dirPath string, yt ytsdk.Client, logger *SimpleLogger) *taskDiscovery {
	return &taskDiscovery{
		baseDomain: baseDomain,
		tablePath:  ypath.Path(dirPath).Child(servicesTableName),
		yt:         yt,

		logger: logger,
	}
}

func (d *taskDiscovery) Discovery(ctx context.Context) (TaskList, error) {
	var tasks []Task

	// TODO: listing all running operations is inefficient
	// Later we will make separate task proxy spec in operations and will request only for operations with it.
	operations, err := d.listOperations(ctx)
	if err != nil {
		return nil, err
	}

	d.logger.Debugf("found %d running operations", len(operations))

	for _, op := range operations {
		title := parseOperationTitle(op)
		annotations := op.RuntimeParameters.Annotations

		var opTasks []Task
		if strings.HasPrefix(title, "Spark driver for") {
			opTasks, err = processSPYTDirectSubmitOperation(op)
			if err != nil {
				d.logger.Errorf("unable to process SPYT direct submit operation %q: %v", op.ID, err)
				continue
			}
		} else if annotations["is_spark"] == true {
			opTasks, err = d.processSPYTStandaloneClusterOperation(ctx, op)
			if err != nil {
				d.logger.Errorf("unable to process SPYT standalone cluster operation %q: %v", op.ID, err)
				continue
			}
		} else if _, ok := annotations[taskProxyAnnotationKey]; ok {
			opTasks, err = d.processTaskProxyAnnotatedOperation(ctx, op)
			if err != nil {
				d.logger.Errorf("unable to process task proxy annotated operation %q: %v", op.ID, err)
				continue
			}
		}
		tasks = append(tasks, opTasks...)
	}
	return tasks, nil
}

func processSPYTDirectSubmitOperation(op ytsdk.OperationStatus) ([]Task, error) {
	descriptionAny, ok := op.RuntimeParameters.Annotations["description"]
	if !ok {
		return nil, fmt.Errorf("no description in operation annotations")
	}
	description, ok := descriptionAny.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("description is not a map")
	}
	webUIany, ok := description["Web UI"]
	if !ok {
		return nil, fmt.Errorf("no Web UI in description")
	}
	webUI, ok := webUIany.(string)
	if !ok {
		return nil, fmt.Errorf("web UI is not a string")
	}
	u, err := url.Parse(webUI)
	if err != nil {
		return nil, fmt.Errorf("invalid SPYT webUI URL format in description: %v", err)
	}
	hostPort, err := makeHostPortFromNode(u.Host)
	if err != nil {
		return nil, fmt.Errorf("unable to make (host, port) from url: %v", err)
	}
	return []Task{
		{
			operationID:    op.ID.String(),
			operationAlias: parseOperationAlias(op),
			taskName:       "driver",
			service:        "ui",
			jobs:           []HostPort{*hostPort},
			protocol:       HTTP,
		},
	}, nil
}

func (d *taskDiscovery) processSPYTStandaloneClusterOperation(ctx context.Context, op ytsdk.OperationStatus) ([]Task, error) {
	descriptionAny, ok := op.RuntimeParameters.Annotations["description"]
	if !ok {
		return nil, fmt.Errorf("no description in operation annotations")
	}
	description, ok := descriptionAny.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("description is not a map")
	}
	sparkAny, ok := description["Spark over YT"]
	if !ok {
		return nil, fmt.Errorf("no Spark over YT in description")
	}
	spark, ok := sparkAny.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Spark over YT is not a map")
	}
	discoveryPathAny, ok := spark["discovery_path"]
	if !ok {
		return nil, fmt.Errorf("no discovery_path in Spark over YT")
	}
	discoveryPath, ok := discoveryPathAny.(string)
	if !ok {
		return nil, fmt.Errorf("discovery_path is not a string")
	}

	var tasks []Task
	for _, t := range []struct {
		taskName string
		dir      string
		service  string
	}{
		{
			taskName: "master",
			dir:      "webui",
			service:  "ui",
		},
		{
			taskName: "master",
			dir:      "rest",
			service:  "rest",
		},
		{
			taskName: "history",
			dir:      "shs",
			service:  "ui",
		},
	} {
		var nodes []string
		listNodeStarted := time.Now()
		err := d.yt.ListNode(ctx, ypath.Path(discoveryPath).Child("discovery").Child(t.dir), &nodes, nil)
		defaultMetrics.ObserveYTDuration("list_node", time.Since(listNodeStarted))
		if err != nil {
			defaultMetrics.ObserveYTError("list_node", err)
			if t.taskName == "history" {
				// history server is optionally enabled in spark conf
				continue
			}
			return nil, fmt.Errorf("failed to list nodes in discovery path for task %q: %v", t.taskName, err)
		}

		var jobs []HostPort
		for _, node := range nodes {
			hostPort, err := makeHostPortFromNode(node)
			if err != nil {
				return nil, fmt.Errorf("unable to make (host, port) from url: %v", err)
			}
			jobs = append(jobs, *hostPort)
		}

		tasks = append(tasks, Task{
			operationID:    op.ID.String(),
			operationAlias: parseOperationAlias(op),
			taskName:       t.taskName,
			service:        t.service,
			jobs:           jobs,
			protocol:       HTTP,
		})
	}
	return tasks, nil
}

func (d *taskDiscovery) processTaskProxyAnnotatedOperation(ctx context.Context, op ytsdk.OperationStatus) ([]Task, error) {
	taskProxyAnnotation := op.RuntimeParameters.Annotations[taskProxyAnnotationKey]
	taskServiceInfos, timeoutOverrides := parseTaskProxyAnnotation(taskProxyAnnotation)
	if taskServiceInfos == nil {
		return nil, fmt.Errorf("invalid task_proxy annotation: %v", taskProxyAnnotation)
	}

	listJobsStarted := time.Now()
	listJobs, err := d.yt.ListJobs(ctx, op.ID, &ytsdk.ListJobsOptions{
		JobState: &ytsdk.JobRunning,
	})
	defaultMetrics.ObserveYTDuration("list_jobs", time.Since(listJobsStarted))
	if err != nil {
		defaultMetrics.ObserveYTError("list_jobs", err)
		return nil, fmt.Errorf("failed to list jobs: %v", err)
	}

	idToTask := make(map[string]*Task)

	for _, job := range listJobs.Jobs {
		var jobPorts []int
		getNodeStarted := time.Now()
		err = d.yt.GetNode(
			ctx,
			ypath.Path(
				fmt.Sprintf(
					"//sys/exec_nodes/%s/orchid/exec_node/job_controller/active_jobs/%s/job_ports",
					job.Address,
					job.ID,
				),
			),
			&jobPorts,
			nil,
		)
		defaultMetrics.ObserveYTDuration("get_node", time.Since(getNodeStarted))
		if err != nil {
			defaultMetrics.ObserveYTError("get_node", err)
			return nil, fmt.Errorf("failed to list job %q ports: %v", job.ID, err)
		}
		for i, port := range jobPorts {
			var serviceInfo *taskServiceInfo
			for _, info := range taskServiceInfos {
				if info.task == job.TaskName && info.portIndex == i {
					serviceInfo = &info
					break
				}
			}
			if serviceInfo == nil {
				serviceInfo = &taskServiceInfo{
					service:  fmt.Sprintf("port%d", i),
					protocol: HTTP,
				}
			}
			hostParts := strings.Split(job.Address, ":") // job address contains port also

			taskProto := Task{
				operationID:      op.ID.String(),
				operationAlias:   parseOperationAlias(op),
				taskName:         job.TaskName,
				service:          serviceInfo.service,
				protocol:         serviceInfo.protocol,
				timeoutOverrides: timeoutOverrides,
			}
			if _, ok := idToTask[taskProto.ID()]; !ok {
				idToTask[taskProto.ID()] = &taskProto
			}

			task, _ := idToTask[taskProto.ID()]
			task.jobs = append(task.jobs, HostPort{
				host: hostParts[0],
				port: uint32(port),
			})
		}
	}

	var tasks []Task
	for _, task := range idToTask {
		if err := task.Validate(); err != nil {
			d.logger.Warnf("invalid task %v: %v", task, err)
		} else {
			tasks = append(tasks, *task)
		}
	}
	return tasks, nil
}

func (d *taskDiscovery) save(ctx context.Context, hashToTask map[string]Task) error {
	nodeExistsStarted := time.Now()
	exists, err := d.yt.NodeExists(ctx, d.tablePath, nil)
	defaultMetrics.ObserveYTDuration("node_exists", time.Since(nodeExistsStarted))
	if err != nil {
		defaultMetrics.ObserveYTError("node_exists", err)
		return err
	}
	if !exists {
		createNodeStarted := time.Now()
		_, err := d.yt.CreateNode(ctx, d.tablePath, ytsdk.NodeTable, nil)
		defaultMetrics.ObserveYTDuration("create_node", time.Since(createNodeStarted))
		if err != nil {
			defaultMetrics.ObserveYTError("create_node", err)
			return err
		}
	}
	writeTableStarted := time.Now()
	w, err := d.yt.WriteTable(ctx, d.tablePath, nil)
	defaultMetrics.ObserveYTDuration("write_table", time.Since(writeTableStarted))
	if err != nil {
		defaultMetrics.ObserveYTError("write_table", err)
		return err
	}
	for hash, task := range hashToTask {
		writeTableRowStarted := time.Now()
		err = w.Write(&TaskRow{
			OperationID: task.operationID,
			TaskName:    task.taskName,
			Service:     task.service,
			Protocol:    string(task.protocol),
			Domain:      getTaskHashDomain(hash, d.baseDomain),
		})
		defaultMetrics.ObserveYTDuration("write_table_row", time.Since(writeTableRowStarted))
		if err != nil {
			defaultMetrics.ObserveYTError("write_table_row", err)
			return err
		}
	}
	writeTableCommitStarted := time.Now()
	if err := w.Commit(); err != nil {
		defaultMetrics.ObserveYTDuration("write_table_commit", time.Since(writeTableCommitStarted))
		defaultMetrics.ObserveYTError("write_table_commit", err)
		return err
	}
	defaultMetrics.ObserveYTDuration("write_table_commit", time.Since(writeTableCommitStarted))
	return nil
}

func (d *taskDiscovery) listOperations(ctx context.Context) ([]ytsdk.OperationStatus, error) {
	var operations []ytsdk.OperationStatus
	var cursor *yson.Time
	limit := 100
	cursorDirection := ytsdk.SortDirectionPast

	for {
		d.logger.Debugf(
			"loading running operations chunk, limit %d, cursor %v, already loaded %d operations",
			limit,
			cursor,
			len(operations),
		)
		listOperationsStarted := time.Now()
		resp, err := d.yt.ListOperations(ctx, &ytsdk.ListOperationsOptions{
			State:           &ytsdk.StateRunning,
			Cursor:          cursor,
			CursorDirection: &cursorDirection,
			Limit:           &limit,
			Attributes:      []string{"id", "runtime_parameters", "brief_spec"},
		})
		defaultMetrics.ObserveYTDuration("list_operations", time.Since(listOperationsStarted))
		if err != nil {
			defaultMetrics.ObserveYTError("list_operations", err)
			return nil, err
		}
		operations = append(operations, resp.Operations...)
		if len(resp.Operations) < limit {
			break
		}
		cursor = &operations[len(operations)-1].StartTime
	}

	return operations, nil
}

type taskServiceInfo struct {
	task      string
	service   string
	protocol  Protocol
	portIndex int
}

func parseTaskProxyAnnotation(taskProxyAny any) ([]taskServiceInfo, TaskTimeoutOverrides) {
	taskProxy, ok := taskProxyAny.(map[string]any)
	if !ok {
		return nil, TaskTimeoutOverrides{}
	}
	enabledAny, ok := taskProxy[taskProxyEnabledKey]
	if !ok {
		return nil, TaskTimeoutOverrides{}
	}
	enabled, ok := enabledAny.(bool)
	if !ok {
		return nil, TaskTimeoutOverrides{}
	}
	if !enabled {
		return nil, TaskTimeoutOverrides{}
	}
	timeoutOverrides, ok := parseTaskTimeoutOverrides(taskProxy)
	if !ok {
		return nil, TaskTimeoutOverrides{}
	}

	taskServiceInfos := make([]taskServiceInfo, 0)
	tasksInfoAny, ok := taskProxy[taskProxyTasksInfoKey]
	if !ok {
		return taskServiceInfos, timeoutOverrides
	}

	tasksInfo, ok := tasksInfoAny.(map[string]any)
	if !ok {
		return taskServiceInfos, timeoutOverrides
	}

	for task, infoAny := range tasksInfo {
		servicesInfo, ok := infoAny.(map[string]any)
		if !ok {
			continue
		}
		for service, infoAny := range servicesInfo {
			info, ok := infoAny.(map[string]any)
			if !ok {
				continue
			}
			protocolAny, ok := info[taskProxyProtocolKey]
			if !ok {
				continue
			}
			protocol, ok := protocolAny.(string)
			if !ok {
				continue
			}
			if protocol != string(HTTP) && protocol != string(GRPC) {
				continue
			}
			portIndexAny, ok := info[taskProxyPortIndexKey]
			if !ok {
				continue
			}
			portIndex64, ok := parseInteger(portIndexAny)
			if !ok {
				continue
			}
			portIndex := int(portIndex64)
			if int64(portIndex) != portIndex64 {
				continue
			}
			taskServiceInfos = append(taskServiceInfos, taskServiceInfo{
				task:      task,
				service:   service,
				protocol:  Protocol(protocol),
				portIndex: portIndex,
			})
		}
	}

	return taskServiceInfos, timeoutOverrides
}

func parseTaskTimeoutOverrides(taskProxy map[string]any) (TaskTimeoutOverrides, bool) {
	var overrides TaskTimeoutOverrides
	if value, ok := taskProxy[taskProxyRouteTimeoutSecondsKey]; ok {
		timeout, ok := parseTimeoutSeconds(value)
		if !ok {
			return TaskTimeoutOverrides{}, false
		}
		overrides.routeTimeout = &timeout
	}
	if value, ok := taskProxy[taskProxyStreamIdleTimeoutKey]; ok {
		timeout, ok := parseTimeoutSeconds(value)
		if !ok {
			return TaskTimeoutOverrides{}, false
		}
		overrides.streamIdleTimeout = &timeout
	}
	return overrides, true
}

func parseTimeoutSeconds(value any) (time.Duration, bool) {
	seconds, ok := parseInteger(value)
	if !ok || seconds < 0 || seconds > int64(math.MaxInt64/time.Second) {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

func parseInteger(value any) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case int32:
		return int64(v), true
	case int16:
		return int64(v), true
	case int8:
		return int64(v), true
	case uint64:
		if v > uint64(math.MaxInt64) {
			return 0, false
		}
		return int64(v), true
	case uint32:
		return int64(v), true
	case uint16:
		return int64(v), true
	case uint8:
		return int64(v), true
	default:
		return 0, false
	}
}

func makeHostPortFromNode(node string) (*HostPort, error) {
	host, port, err := net.SplitHostPort(node)
	if err != nil {
		return nil, err
	}
	portI, err := strconv.Atoi(port)
	if err != nil {
		return nil, err
	}
	return &HostPort{
		host: host,
		port: uint32(portI),
	}, nil
}

func parseOperationTitle(op ytsdk.OperationStatus) string {
	titleAny, ok := op.BriefSpec["title"]
	if !ok {
		return ""
	}
	title, ok := titleAny.(string)
	if !ok {
		return ""
	}
	return title
}

func parseOperationAlias(op ytsdk.OperationStatus) string {
	aliasAny, ok := op.BriefSpec["alias"]
	if !ok {
		return ""
	}
	alias, ok := aliasAny.(string)
	if !ok {
		return ""
	}
	return alias[1:] // YT operation alias must start with '*', but we skip it to use alias in domains
}
