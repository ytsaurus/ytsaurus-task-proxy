package pkg

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"go.ytsaurus.tech/yt/go/ypath"
	"go.ytsaurus.tech/yt/go/yson"
	ytsdk "go.ytsaurus.tech/yt/go/yt"
)

const servicesTableName = "services"

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
		} else if _, ok := annotations["task_proxy"]; ok {
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
			operationID: op.ID.String(),
			taskName:    "driver",
			service:     "ui",
			jobs:        []HostPort{*hostPort},
			protocol:    HTTP,
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
		err := d.yt.ListNode(ctx, ypath.Path(discoveryPath).Child("discovery").Child(t.dir), &nodes, nil)
		if err != nil {
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
			operationID: op.ID.String(),
			taskName:    t.taskName,
			service:     t.service,
			jobs:        jobs,
			protocol:    HTTP,
		})
	}
	return tasks, nil
}

func (d *taskDiscovery) processTaskProxyAnnotatedOperation(ctx context.Context, op ytsdk.OperationStatus) ([]Task, error) {
	taskProxyAnnotation := op.RuntimeParameters.Annotations["task_proxy"]
	taskServiceInfos := parseTaskProxyAnnotation(taskProxyAnnotation)
	if taskServiceInfos == nil {
		return nil, fmt.Errorf("invalid task_proxy annotation: %v", taskProxyAnnotation)
	}

	listJobs, err := d.yt.ListJobs(ctx, op.ID, &ytsdk.ListJobsOptions{
		JobState: &ytsdk.JobRunning,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list jobs: %v", err)
	}

	idToTask := make(map[string]*Task)

	for _, job := range listJobs.Jobs {
		var jobPorts []int
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
		if err != nil {
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
					service:  fmt.Sprintf("port_%d", i),
					protocol: HTTP,
				}
			}
			hostParts := strings.Split(job.Address, ":") // job address contains port also

			taskProto := Task{
				operationID: op.ID.String(),
				taskName:    job.TaskName,
				service:     serviceInfo.service,
				protocol:    serviceInfo.protocol,
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
		tasks = append(tasks, *task)
	}
	return tasks, nil
}

func (d *taskDiscovery) save(ctx context.Context, hashToTask map[string]Task) error {
	exists, err := d.yt.NodeExists(ctx, d.tablePath, nil)
	if err != nil {
		return err
	}
	if !exists {
		_, err := d.yt.CreateNode(ctx, d.tablePath, ytsdk.NodeTable, nil)
		if err != nil {
			return err
		}
	}
	w, err := d.yt.WriteTable(ctx, d.tablePath, nil)
	if err != nil {
		return err
	}
	for hash, task := range hashToTask {
		err = w.Write(&TaskRow{
			OperationID: task.operationID,
			TaskName:    task.taskName,
			Service:     task.service,
			Protocol:    string(task.protocol),
			Domain:      getTaskDomain(hash, d.baseDomain),
		})
		if err != nil {
			return err
		}
	}
	return w.Commit()
}

func (d *taskDiscovery) listOperations(ctx context.Context) ([]ytsdk.OperationStatus, error) {
	var operations []ytsdk.OperationStatus
	var cursor *yson.Time
	limit := 100
	cursorDirection := ytsdk.SortDirectionPast

	for {
		d.logger.Debugf(
			"loading running operations chunk, limit %d, cursor %s, already loaded %d operations",
			limit,
			cursor,
			len(operations),
		)
		resp, err := d.yt.ListOperations(ctx, &ytsdk.ListOperationsOptions{
			State:           &ytsdk.StateRunning,
			Cursor:          cursor,
			CursorDirection: &cursorDirection,
			Limit:           &limit,
			Attributes:      []string{"id", "runtime_parameters", "brief_spec"},
		})
		if err != nil {
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

func parseTaskProxyAnnotation(taskProxyAny any) []taskServiceInfo {
	taskProxy, ok := taskProxyAny.(map[string]any)
	if !ok {
		return nil
	}
	enabledAny, ok := taskProxy["enabled"]
	if !ok {
		return nil
	}
	enabled, ok := enabledAny.(bool)
	if !ok {
		return nil
	}
	if !enabled {
		return nil
	}

	taskServiceInfos := make([]taskServiceInfo, 0)
	tasksInfoAny, ok := taskProxy["tasks_info"]
	if !ok {
		return taskServiceInfos
	}

	tasksInfo, ok := tasksInfoAny.(map[string]any)
	if !ok {
		return taskServiceInfos
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
			protocolAny, ok := info["protocol"]
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
			portIndexAny, ok := info["port_index"]
			if !ok {
				continue
			}
			var portIndex int
			switch v := portIndexAny.(type) {
			case int:
				portIndex = v
			case int64:
				portIndex = int(v)
			case int32:
				portIndex = int(v)
			case int16:
				portIndex = int(v)
			case int8:
				portIndex = int(v)
			case uint64:
				portIndex = int(v)
			case uint32:
				portIndex = int(v)
			case uint16:
				portIndex = int(v)
			case uint8:
				portIndex = int(v)
			default:
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

	return taskServiceInfos
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
