package pkg

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"go.ytsaurus.tech/yt/go/ypath"
	ytsdk "go.ytsaurus.tech/yt/go/yt"
)

type TaskDiscovery struct {
	BaseDomain string
	TablePath  string
	YT         ytsdk.Client

	Logger *SimpleLogger
}

func (d *TaskDiscovery) Discovery(ctx context.Context) (TaskList, error) {
	var tasks []Task

	result, err := d.YT.ListOperations(ctx, &ytsdk.ListOperationsOptions{
		State: &ytsdk.StateRunning,
	})
	if err != nil {
		return nil, err
	}

	d.Logger.Debugf("found %d running operations", len(result.Operations))

	for _, op := range result.Operations {
		title := parseOperationTitle(op)
		annotations := op.RuntimeParameters.Annotations

		var opTasks []Task
		if strings.HasPrefix(title, "Spark driver for") {
			opTasks, err = processSPYTdirectSubmitOperation(op)
			if err != nil {
				d.Logger.Errorf("unable to process SPYT direct submit operation %q: %v", op.ID, err)
				continue
			}
		} else if annotations["is_spark"] == true {
			opTasks, err = d.processSPYTstandloneClusterOperation(ctx, op)
			if err != nil {
				d.Logger.Errorf("unable to process SPYT standalone cluster operation %q: %v", op.ID, err)
				continue
			}
		} else if _, ok := annotations["task_proxy"]; ok {
			opTasks, err = d.processTaskProxyAnnotatedOperation(ctx, op)
			if err != nil {
				d.Logger.Errorf("unable to process task proxy annotated operation %q: %v", op.ID, err)
				continue
			}
		}
		tasks = append(tasks, opTasks...)
	}
	return tasks, nil
}

func processSPYTdirectSubmitOperation(op ytsdk.OperationStatus) ([]Task, error) {
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

func (d *TaskDiscovery) processSPYTstandloneClusterOperation(ctx context.Context, op ytsdk.OperationStatus) ([]Task, error) {
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
	}{
		{
			taskName: "master",
			dir:      "webui",
		},
		{
			taskName: "history",
			dir:      "shs",
		},
	} {
		var nodes []string
		err := d.YT.ListNode(ctx, ypath.Path(discoveryPath).Child("discovery").Child(t.dir), &nodes, nil)
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
			service:     "ui",
			jobs:        jobs,
			protocol:    HTTP,
		})
	}
	return tasks, nil
}

func (d *TaskDiscovery) processTaskProxyAnnotatedOperation(ctx context.Context, op ytsdk.OperationStatus) ([]Task, error) {
	taskProxyAnnotation := op.RuntimeParameters.Annotations["task_proxy"]
	taskServiceInfos := parseTaskProxyAnnotation(taskProxyAnnotation)
	if taskServiceInfos == nil {
		return nil, fmt.Errorf("invalid task_proxy annotation: %v", taskProxyAnnotation)
	}

	listJobs, err := d.YT.ListJobs(ctx, op.ID, &ytsdk.ListJobsOptions{
		JobState: &ytsdk.JobRunning,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list jobs: %v", err)
	}

	idToTask := make(map[string]*Task)

	for _, job := range listJobs.Jobs {
		var jobPorts []int
		err = d.YT.GetNode(
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

func (d *TaskDiscovery) Save(ctx context.Context, hashToTask map[string]Task) error {
	tableYPath := ypath.Path(d.TablePath)
	exists, err := d.YT.NodeExists(ctx, tableYPath, nil)
	if err != nil {
		return err
	}
	if !exists {
		_, err := d.YT.CreateNode(ctx, tableYPath, ytsdk.NodeTable, nil)
		if err != nil {
			return err
		}
	}
	w, err := d.YT.WriteTable(ctx, tableYPath, nil)
	if err != nil {
		return err
	}
	for hash, task := range hashToTask {
		err = w.Write(&TaskRow{
			OperationID: task.operationID,
			TaskName:    task.taskName,
			Service:     task.service,
			Protocol:    string(task.protocol),
			Domain:      getTaskDomain(hash, d.BaseDomain),
		})
		if err != nil {
			return err
		}
	}
	return w.Commit()
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
