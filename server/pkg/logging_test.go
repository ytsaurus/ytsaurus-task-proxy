package pkg

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoggingFiltersLevelsAndSelectsOutput(t *testing.T) {
	for _, writer := range []string{"stdout", "stderr"} {
		t.Run(writer, func(t *testing.T) {
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			previous := os.Stderr
			if writer == "stdout" {
				previous = os.Stdout
				os.Stdout = write
			} else {
				os.Stderr = write
			}
			defer func() {
				if writer == "stdout" {
					os.Stdout = previous
				} else {
					os.Stderr = previous
				}
			}()
			cfg := DefaultLoggingConfig()
			cfg.Server.WriterType = writer
			cfg.Server.MinLogLevel = "warning"
			logging, err := NewLogging(cfg)
			if err != nil {
				t.Fatal(err)
			}
			logger := logging.Logger()
			logger.Tracef("hidden trace")
			logger.Debugf("hidden debug")
			logger.Infof("hidden info")
			logger.Warnf("visible warning")
			logger.Errorf("visible error")
			if err = logging.Close(); err != nil {
				t.Fatal(err)
			}
			write.Close()
			out, err := io.ReadAll(read)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(out), "hidden") || !strings.Contains(string(out), "visible warning") || !strings.Contains(string(out), "visible error") {
				t.Fatalf("output: %s", out)
			}
		})
	}
}

func TestLoggingRunStopsAndCloseIsIdempotent(t *testing.T) {
	logging, err := NewLogging(DefaultLoggingConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { logging.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
	if err = logging.Close(); err != nil {
		t.Fatal(err)
	}
	if err = logging.Close(); err != nil {
		t.Fatal(err)
	}
}
