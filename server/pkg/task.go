package pkg

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

type Protocol string

const (
	HTTP Protocol = "http"
	GRPC Protocol = "grpc"
)

type HostPort struct {
	host string
	port uint32
}

type Task struct {
	operationID string
	taskName    string
	service     string
	protocol    Protocol
	jobs        []HostPort
}

// Identifies task, for sorting and domain hash
func (t *Task) ID() string {
	return t.operationID + t.taskName + t.service
}

// ID with jobs (host, port)-s to create correct version for xDS data (jobs can move between hosts)
func (t *Task) IDWithHostPort() string {
	sb := strings.Builder{}
	sb.WriteString(t.ID())
	for _, job := range t.jobs {
		sb.WriteString(job.host)
		fmt.Fprintf(&sb, "%d", job.port)
	}
	return sb.String()
}

type TaskRow struct {
	OperationID string `yson:"operation_id"`
	TaskName    string `yson:"task_name"`
	Service     string `yson:"service"`
	Protocol    string `yson:"protocol"`
	Domain      string `yson:"domain"`
}

func getTaskDomain(taskHash, baseDomain string) string {
	return taskHash + "." + baseDomain
}

func Hash(source []byte) string {
	hash := fmt.Sprintf("%x", sha256.Sum256(source))
	return hash[len(hash)-8:]
}

type TaskList []Task

func (a TaskList) Len() int           { return len(a) }
func (a TaskList) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a TaskList) Less(i, j int) bool { return a[i].ID() < a[j].ID() }

func (a TaskList) String() string {
	sb := strings.Builder{}
	for _, task := range a {
		sb.WriteString(fmt.Sprintf("\t%v\n", task))
	}
	return sb.String()
}
