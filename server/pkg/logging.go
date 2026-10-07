package pkg

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

var logLevels = map[string]int{"trace": 0, "debug": 1, "info": 2, "warning": 3, "error": 4}

type logState struct {
	mu      sync.Mutex
	writer  io.Writer
	minimum int
	closed  bool
}

// SimpleLogger keeps its zero-value stderr/debug behavior and value-method xDS compatibility.
type SimpleLogger struct{ state *logState }

func (l SimpleLogger) write(level int, label, format string, args ...any) {
	if l.state == nil {
		if level >= logLevels["debug"] {
			log.Printf(label+": "+format, args...)
		}
		return
	}
	s := l.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if level < s.minimum || s.closed {
		return
	}
	record := time.Now().UTC().Format(time.RFC3339Nano) + " " + label + ": " + fmt.Sprintf(format, args...) + "\n"
	if _, err := io.WriteString(s.writer, record); err != nil {
		loggingError("write server log: %v", err)
	}
}
func (l SimpleLogger) Tracef(format string, args ...any) { l.write(0, "TRACE", format, args...) }
func (l SimpleLogger) Debugf(format string, args ...any) { l.write(1, "DEBUG", format, args...) }
func (l SimpleLogger) Infof(format string, args ...any)  { l.write(2, "INFO", format, args...) }
func (l SimpleLogger) Warnf(format string, args ...any)  { l.write(3, "WARN", format, args...) }
func (l SimpleLogger) Errorf(format string, args ...any) { l.write(4, "ERROR", format, args...) }

// Logging owns server output and the shared rotation lifecycle. External files are opened by Envoy.
type Logging struct {
	state      *logState
	logger     *SimpleLogger
	streams    []*rotatingLog
	rotationMu sync.Mutex
	done       chan struct{}
	closed     bool
	reopen     func() error
}

func NewLogging(config LoggingConfig) (*Logging, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	l := &Logging{state: &logState{minimum: logLevels[config.Server.MinLogLevel]}, done: make(chan struct{})}
	l.logger = &SimpleLogger{state: l.state}
	l.reopen = reopenEnvoyLogs
	switch config.Server.WriterType {
	case "stdout":
		l.state.writer = os.Stdout
	case "stderr":
		l.state.writer = os.Stderr
	}
	entries := []struct {
		path, writer      string
		policy            *RotationPolicy
		external, enabled bool
	}{
		{config.ServerLogPath(), config.Server.WriterType, config.Server.RotationPolicy, false, true},
		{config.ProxyLogPath(), config.Proxy.WriterType, config.Proxy.RotationPolicy, true, true},
		{config.AccessLogPath(), config.AccessLog.WriterType, config.AccessLog.RotationPolicy, true, config.AccessLog.Enabled},
	}
	for _, entry := range entries {
		if !entry.enabled || entry.writer != "file" {
			continue
		}
		if err := os.MkdirAll(config.Directory, 0750); err != nil {
			l.Close()
			return nil, err
		}
		dir, err := os.Lstat(config.Directory)
		if err != nil || !dir.IsDir() {
			l.Close()
			return nil, fmt.Errorf("log directory must be a real directory: %s", config.Directory)
		}
		// Copy policies: callers cannot mutate running rotation via config pointers.
		policy := *entry.policy
		stream := &rotatingLog{path: entry.path, policy: policy, external: entry.external, since: time.Now()}
		info, err := regularLogInfo(entry.path)
		if err != nil && !os.IsNotExist(err) {
			l.Close()
			return nil, err
		}
		if info != nil {
			stream.since = info.ModTime()
		}
		if entry.external {
			archives, err := stream.archives()
			if err != nil {
				l.Close()
				return nil, err
			}
			// An empty final-format file can be the name reservation left by a
			// crash before rename. Only a nonempty archive can hold prior Envoy
			// records; never let a reservation displace its protection.
			for i := len(archives) - 1; i >= 0; i-- {
				if archives[i].size > 0 {
					stream.protected = archives[i].path
					if info == nil {
						stream.pending = stream.protected
					}
					break
				}
			}
		} else {
			stream.file, err = openLogFile(entry.path)
			if err != nil {
				l.Close()
				return nil, err
			}
			l.state.writer = stream.file
		}
		l.streams = append(l.streams, stream)
	}
	return l, nil
}

func (l *Logging) Logger() *SimpleLogger { return l.logger }
func (l *Logging) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.done:
			return
		case now := <-ticker.C:
			l.rotate(now)
		}
	}
}
func (l *Logging) Close() error {
	l.rotationMu.Lock()
	defer l.rotationMu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	close(l.done)
	l.state.mu.Lock()
	defer l.state.mu.Unlock()
	l.state.closed = true
	var result error
	for _, stream := range l.streams {
		if stream.file != nil {
			if err := stream.file.Close(); err != nil {
				result = err
			}
		}
	}
	return result
}
func loggingError(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "task-proxy logging: "+format+"\n", args...)
}
func reopenEnvoyLogs() error {
	client := http.Client{Timeout: 2 * time.Second}
	response, err := client.Post("http://127.0.0.1:9901/reopen_logs", "text/plain", nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	// Bound response draining; no unbounded admin response allocation.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Envoy reopen_logs returned HTTP %d", response.StatusCode)
	}
	return nil
}
