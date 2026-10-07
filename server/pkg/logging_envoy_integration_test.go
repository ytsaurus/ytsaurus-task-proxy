//go:build integration

package pkg

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

// This deliberately uses a real Envoy and production rotation backend. The
// generated xDS resources become static bootstrap resources to avoid requiring
// YTsaurus or a Kubernetes cluster. Listener access logs retain their production
// placement; an unmatched filter chain supplies the listener failure event.
func TestEnvoyLoggingIntegration(t *testing.T) {
	require.NoError(t, exec.Command("docker", "info").Run(), "Docker must be available; run make test-integration explicitly")
	// The production rotator addresses the local Envoy admin port.
	probe, err := net.Listen("tcp", "127.0.0.1:9901")
	require.NoError(t, err, "integration requires host port 9901 to be free")
	require.NoError(t, probe.Close())
	root, err := filepath.Abs("../../.superpowers")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(root, 0755))
	directory, err := os.MkdirTemp(root, "envoy-integration-")
	require.NoError(t, err)
	defer os.RemoveAll(directory)
	require.NoError(t, os.WriteFile(filepath.Join(directory, ".gitignore"), []byte("*\n"), 0644))
	require.NoError(t, os.Chmod(directory, 0777), "bind mount substitutes for the fsGroup-writable PVC")
	config := DefaultLoggingConfig()
	config.Directory = directory
	policy := &RotationPolicy{MaxSegmentSize: 1, MaxSegmentCountToKeep: 3}
	config.Server = LogConfig{WriterType: "file", MinLogLevel: "debug", RotationPolicy: policy}
	config.Proxy = LogConfig{WriterType: "file", MinLogLevel: "debug", RotationPolicy: policy}
	config.AccessLog = AccessLogConfig{Enabled: true, WriterType: "file", RotationPolicy: policy}
	snapshot, err := makeSnapshot(map[string]Task{}, "integration", "example.com", false, false, DefaultTaskProxyTimeoutConfig(), config.AccessLog, config.AccessLogPath())
	require.NoError(t, err)
	listener := snapshot.GetResources(resourcev3.ListenerType)["listener_0"].(*listenerv3.Listener)
	// Listener access logging is for listener events, rather than HTTP requests.
	// Force a rejected connection without changing the configured logging sink.
	listener.FilterChains[0].FilterChainMatch = &listenerv3.FilterChainMatch{ApplicationProtocols: []string{"integration-unmatched"}}
	static := map[string]any{}
	for kind, key := range map[resourcev3.Type]string{resourcev3.ListenerType: "listeners", resourcev3.ClusterType: "clusters"} {
		resources := []json.RawMessage{}
		for _, resource := range snapshot.GetResources(kind) {
			data, err := protojson.Marshal(resource)
			require.NoError(t, err)
			resources = append(resources, json.RawMessage(data))
		}
		static[key] = resources
	}
	bootstrap := map[string]any{"static_resources": static, "admin": map[string]any{"address": map[string]any{"socket_address": map[string]any{"address": "0.0.0.0", "port_value": 9901}}}}
	encoded, err := json.Marshal(bootstrap)
	require.NoError(t, err)
	bootstrapPath := filepath.Join(directory, "envoy.json")
	require.NoError(t, os.WriteFile(bootstrapPath, encoded, 0644))
	name := fmt.Sprintf("task-proxy-logging-%d", time.Now().UnixNano())
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		require.NoError(t, err, "docker %v: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	image := os.Getenv("TASK_PROXY_ENVOY_IMAGE")
	if image == "" {
		image = "envoyproxy/envoy:v1.36-latest"
	}
	start := func() string {
		t.Helper()
		docker("run", "-d", "--name", name, "-p", "127.0.0.1:9901:9901", "-p", "127.0.0.1::8080", "-v", directory+":"+directory, image, "envoy", "-c", bootstrapPath, "--log-path", config.ProxyLogPath(), "--log-level", "debug", "--concurrency", "1")
		require.Eventually(t, func() bool {
			response, err := http.Get("http://127.0.0.1:9901/ready")
			if err != nil {
				return false
			}
			defer response.Body.Close()
			return response.StatusCode == 200
		}, 15*time.Second, 100*time.Millisecond, "Envoy should accept generated xDS resources")
		var ports map[string][]struct{ HostPort string }
		require.NoError(t, json.Unmarshal([]byte(docker("inspect", name, "--format", "{{json .NetworkSettings.Ports}}")), &ports))
		require.Len(t, ports["8080/tcp"], 1)
		address := "127.0.0.1:" + ports["8080/tcp"][0].HostPort
		require.Eventually(t, func() bool {
			connection, err := net.DialTimeout("tcp", address, time.Second)
			if err != nil {
				return false
			}
			connection.Close()
			return true
		}, 15*time.Second, 100*time.Millisecond, "Envoy listener port becomes reachable")
		return address
	}
	defer func() {
		if t.Failed() {
			out, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Logf("Envoy logs: %s", out)
		}
		exec.Command("docker", "rm", "-f", name).Run()
	}()
	address := start()
	emit := func() {
		t.Helper()
		connection, err := net.DialTimeout("tcp", address, 2*time.Second)
		require.NoError(t, err)
		connection.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.WriteString(connection, "GET /probe HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n")
		_, _ = io.ReadAll(connection)
		connection.Close()
	}
	nonempty := func(path string) bool { st, err := os.Stat(path); return err == nil && st.Size() > 0 }
	emit()
	require.Eventually(t, func() bool { return nonempty(config.AccessLogPath()) }, 20*time.Second, 100*time.Millisecond, "real generated Listener access sink must write rejected connection")
	logging, err := NewLogging(config)
	require.NoError(t, err)
	defer logging.Close()
	logging.Logger().Infof("server-before-rotation")
	oldInfos := map[string]os.FileInfo{}
	for _, path := range []string{config.ServerLogPath(), config.ProxyLogPath(), config.AccessLogPath()} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		oldInfos[path] = info
	}
	logging.rotate(time.Now())
	emit()
	require.Eventually(t, func() bool {
		return nonempty(config.AccessLogPath()) && nonempty(config.ProxyLogPath())
	}, 20*time.Second, 100*time.Millisecond, "Envoy creates active paths after reopen and subsequent records")
	for _, path := range []string{config.ServerLogPath(), config.ProxyLogPath(), config.AccessLogPath()} {
		info, err := os.Stat(path)
		require.NoError(t, err, "reopen must create the active file: %s", path)
		require.False(t, os.SameFile(oldInfos[path], info), "real reopen must replace the inode: %s", path)
		archives, err := filepath.Glob(path + ".*")
		require.NoError(t, err)
		require.NotEmpty(t, archives, "archive must survive reopen: %s", path)
	}
	emit()
	logging.Logger().Infof("server-after-rotation")
	require.Eventually(t, func() bool { return nonempty(config.AccessLogPath()) && nonempty(config.ProxyLogPath()) }, 20*time.Second, 100*time.Millisecond, "both Envoy files must receive records after real admin reopen")
	// Replacement reuses the same retained filesystem. Verify active appending and
	// preserved archives, then rotate repeatedly to exercise production retention.
	before, _ := os.ReadFile(config.AccessLogPath())
	archivesBefore, _ := filepath.Glob(config.AccessLogPath() + ".*")
	require.NoError(t, logging.Close())
	docker("rm", "-f", name)
	address = start()
	logging, err = NewLogging(config)
	require.NoError(t, err)
	defer logging.Close()
	emit()
	require.Eventually(t, func() bool {
		current, _ := os.ReadFile(config.AccessLogPath())
		return len(current) > len(before) && strings.HasPrefix(string(current), string(before))
	}, 20*time.Second, 100*time.Millisecond, "replacement appends to retained active file")
	for _, archive := range archivesBefore {
		_, err := os.Stat(archive)
		require.NoError(t, err, "replacement preserves old archive")
	}
	for i := 0; i < 4; i++ {
		logging.Logger().Infof("retention-cycle-%d", i)
		emit()
		require.Eventually(t, func() bool { return nonempty(config.AccessLogPath()) }, 20*time.Second, 100*time.Millisecond)
		logging.rotate(time.Now())
		emit()
		require.Eventually(t, func() bool { return nonempty(config.AccessLogPath()) && nonempty(config.ProxyLogPath()) }, 20*time.Second, 100*time.Millisecond)
		// Reconcile pending reopen after Envoy has recreated active paths. This
		// mirrors the next tick of Logging.Run, without flaky wall-clock rotation.
		logging.rotate(time.Now())
		for _, stream := range logging.streams {
			require.Empty(t, stream.pending, "successful real reopen reconciles pending archive")
		}
	}
	for _, path := range []string{config.ServerLogPath(), config.ProxyLogPath(), config.AccessLogPath()} {
		archives, err := filepath.Glob(path + ".*")
		require.NoError(t, err)
		require.Len(t, archives, 3, "production retention prunes older archives to the configured count")
	}
	t.Logf("PASS: actual backend + Envoy %s generated Listener file sink, rename/admin reopen, replacement appending, archive retention", image)
}
