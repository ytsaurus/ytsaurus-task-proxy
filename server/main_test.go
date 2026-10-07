package main

import (
	"bytes"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestServerProcessHelper(t *testing.T) {
	if os.Getenv("TASK_PROXY_PROCESS_TEST") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			break
		}
	}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	main()
	os.Exit(0)
}

func processCommand(args ...string) *exec.Cmd {
	command := exec.Command(os.Args[0], append([]string{"-test.run=^TestServerProcessHelper$", "--"}, args...)...)
	command.Env = append(os.Environ(), "TASK_PROXY_PROCESS_TEST=1")
	return command
}

func writeProcessLoggingConfig(t *testing.T, writer string) (string, string) {
	t.Helper()
	directory := t.TempDir()
	config := fmt.Sprintf("directory: %q\nserver:\n  writerType: %s\n  minLogLevel: info\n", directory, writer)
	if writer == "file" {
		config += "  rotationPolicy:\n    maxSegmentSize: 1\n    maxSegmentCountToKeep: 10\n"
	}
	path := filepath.Join(directory, "logging.yaml")
	require.NoError(t, os.WriteFile(path, []byte(config), 0600))
	return path, filepath.Join(directory, "server.log")
}

func TestProcessOperationalErrorsUseConfiguredLogger(t *testing.T) {
	for _, writer := range []string{"stdout", "stderr", "file"} {
		t.Run(writer, func(t *testing.T) {
			configPath, logPath := writeProcessLoggingConfig(t, writer)
			command := processCommand("-logging-config", configPath)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			require.Error(t, command.Run(), "invalid operational arguments must exit unsuccessfully")
			var output []byte
			switch writer {
			case "stdout":
				output = stdout.Bytes()
				require.Empty(t, stderr.String())
			case "stderr":
				output = stderr.Bytes()
				require.Empty(t, stdout.String())
			case "file":
				var err error
				output, err = os.ReadFile(logPath)
				require.NoError(t, err)
				require.Empty(t, stdout.String())
				require.Empty(t, stderr.String())
			}
			require.Contains(t, string(output), "ERROR: 'yt-proxy' argument is required")
		})
	}
}

func TestProcessLoggingInitializationErrorsUseStderr(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "logging.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("server:\n  writerType: unsupported\n"), 0600))
	command := processCommand("-logging-config", configPath)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	require.Error(t, command.Run())
	require.Empty(t, stdout.String())
	require.Contains(t, stderr.String(), "failed to load logging configuration")
	require.Contains(t, stderr.String(), "invalid writerType")
}

func TestProcessRunsRotationAndStopsOnSignal(t *testing.T) {
	// The service's fixed ports are part of its existing deployment contract.
	for _, address := range []string{":9090", ":9102"} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Skipf("service port unavailable: %v", err)
		}
		require.NoError(t, listener.Close())
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "test YT unavailable", http.StatusServiceUnavailable)
	}))
	defer proxy.Close()
	configPath, logPath := writeProcessLoggingConfig(t, "file")
	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-token"), 0600))
	command := processCommand("-logging-config", configPath, "-yt-proxy", strings.TrimPrefix(proxy.URL, "http://"), "-yt-token-path", tokenPath, "-base-domain", "example.com", "-dir-path", "//task-proxy")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	require.NoError(t, command.Start())
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	require.Eventually(t, func() bool {
		data, _ := os.ReadFile(logPath)
		return strings.Contains(string(data), "xDS + extAuthz starts listening") && strings.Contains(string(data), "metrics HTTP starts listening")
	}, 5*time.Second, 20*time.Millisecond, "configured server log should contain both startup messages")
	require.Eventually(t, func() bool {
		archives, _ := filepath.Glob(logPath + ".*")
		return len(archives) > 0
	}, 5*time.Second, 20*time.Millisecond, "the process must run periodic rotation")
	require.NoError(t, command.Process.Signal(syscall.SIGTERM))
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		waited = true
		require.NoError(t, err, "SIGTERM should allow logging cleanup")
	case <-time.After(5 * time.Second):
		t.Fatal("process did not stop on SIGTERM")
	}
	require.Empty(t, stdout.String())
	require.Empty(t, stderr.String())
}
