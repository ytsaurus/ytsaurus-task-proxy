package pkg

import (
	"os"
	"path/filepath"
	"testing"
)

func loadTestLogging(t *testing.T, content string) (LoggingConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "logging.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return LoadLoggingConfig(path)
}

func TestLoggingConfigPartialDefaultsAndQuantities(t *testing.T) {
	cfg, err := loadTestLogging(t, `server:
  writerType: file
  minLogLevel: warning
  rotationPolicy:
    maxSegmentSize: 1.5Mi
    maxTotalSizeToKeep: 2G
accessLog:
  enabled: false
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxy.WriterType != "stderr" || cfg.Proxy.MinLogLevel != "info" || cfg.AccessLog.Enabled {
		t.Fatalf("defaults: %+v", cfg)
	}
	if cfg.Server.RotationPolicy.MaxSegmentSize != 1572864 || cfg.Server.RotationPolicy.MaxTotalSizeToKeep != 2000000000 {
		t.Fatalf("quantities: %+v", cfg.Server.RotationPolicy)
	}
	if cfg.ServerLogPath() != "/var/log/task-proxy/server.log" || cfg.ProxyLogPath() != "/dev/stderr" {
		t.Fatal("wrong resolved paths")
	}
}

func TestLoggingConfigRejectsInvalidInput(t *testing.T) {
	for _, input := range []string{
		"unknown: true", "server: {writerType: invalid}", "server: {minLogLevel: fatal}",
		"server: {rotationPolicy: {maxSegmentSize: 10, maxSegmentCountToKeep: 1}}",
		"server: {writerType: file}",
		"server: {writerType: file, rotationPolicy: {maxSegmentSize: 10}}",
		"server: {writerType: file, rotationPolicy: {maxSegmentCountToKeep: 1}}",
		"server: {writerType: file, rotationPolicy: {maxSegmentSize: 0, maxSegmentCountToKeep: 1}}",
		"server: {writerType: file, rotationPolicy: {maxSegmentSize: 999999999999999999999Gi, maxSegmentCountToKeep: 1}}",
		"server: {writerType: file, rotationPolicy: {rotationPeriodMilliseconds: 9223372036854775807, maxSegmentCountToKeep: 1}}",
		"server: {writerType: file, rotationPolicy: {maxSegmentSize: 10, typo: 1}}",
		"server: {writerType: file, rotationPolicy: {maxSegmentSize: true, maxSegmentCountToKeep: 1}}",
		"server: null", "null", "{}\n---\n{}", "{}\n---\n", "server: {writerType: stdout, writerType: stderr}",
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := loadTestLogging(t, input); err == nil {
				t.Fatalf("accepted invalid config: %s", input)
			}
		})
	}
}
