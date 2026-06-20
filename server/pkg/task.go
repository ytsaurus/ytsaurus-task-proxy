package pkg

import (
	"crypto/sha256"
	"fmt"
	"regexp"
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
	operationID    string
	operationAlias string
	taskName       string
	service        string
	protocol       Protocol
	jobs           []HostPort
}

// taskName and service must stay hyphen-free: they are the last two segments of the
// alias subdomain (<alias>-<task>-<service>) and parsing relies on that.
var valueRegexp = regexp.MustCompile(`^[a-z0-9_]{1,30}$`)

// The alias may contain hyphens (strawberry aliases commonly do). Edge hyphens are
// disallowed so the resulting DNS label stays valid; tryParseAliasSubdomain recovers
// the alias by taking everything left of the trailing -<task>-<service>.
var aliasRegexp = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]{0,28}[a-z0-9_])?$`)

// Identifies task, for sorting and domain hash
func (t *Task) ID() string {
	return t.operationID + t.taskName + t.service
}

func (t *Task) Hash() string {
	return Hash([]byte(t.ID()))
}

func (t *Task) OperationID() string {
	return t.operationID
}

func (t *Task) OperationAlias() string {
	return t.operationAlias
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

func (t *Task) Validate() error {
	if t.operationAlias == "" {
		return nil
	}
	// to avoid collisions in alias domains, we should check some fields on regexp
	for _, f := range []struct {
		value  string
		name   string
		regexp *regexp.Regexp
	}{
		{
			value:  t.operationAlias,
			name:   "operationAlias",
			regexp: aliasRegexp,
		},
		{
			value:  t.taskName,
			name:   "taskName",
			regexp: valueRegexp,
		},
		{
			value:  t.service,
			name:   "service",
			regexp: valueRegexp,
		},
	} {
		if !f.regexp.MatchString(f.value) {
			return fmt.Errorf("field %q value %q does not match regexp %q", f.name, f.value, f.regexp.String())
		}
	}
	return nil
}

type TaskRow struct {
	OperationID string `yson:"operation_id"`
	TaskName    string `yson:"task_name"`
	Service     string `yson:"service"`
	Protocol    string `yson:"protocol"`
	Domain      string `yson:"domain"`
}

func getTaskHashDomain(taskHash, baseDomain string) string {
	return fmt.Sprintf("%s.%s", taskHash, baseDomain)
}

func getTaskAliasDomain(task Task, baseDomain string) string {
	return fmt.Sprintf("%s-%s-%s.%s", task.operationAlias, task.taskName, task.service, baseDomain)
}

func tryParseAliasSubdomain(subdomain string) (string, string, string, bool) {
	// Format: <alias>-<task>-<service>. The alias may itself contain hyphens, so we
	// parse from the right: task and service are the last two (hyphen-free) segments,
	// and everything before them is the alias.
	parts := strings.Split(subdomain, "-")
	if len(parts) < 3 {
		return "", "", "", false
	}
	n := len(parts)
	alias := strings.Join(parts[:n-2], "-")
	return alias, parts[n-2], parts[n-1], true
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
		fmt.Fprintf(&sb, "\t%v\n", task)
	}
	return sb.String()
}
