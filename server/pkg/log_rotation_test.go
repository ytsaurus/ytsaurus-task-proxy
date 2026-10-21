package pkg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fileLoggingConfig(t *testing.T) LoggingConfig {
	t.Helper()
	cfg := DefaultLoggingConfig()
	cfg.Directory = t.TempDir()
	cfg.Server.WriterType = "file"
	cfg.Server.RotationPolicy = &RotationPolicy{MaxSegmentSize: 1, MaxSegmentCountToKeep: 2}
	return cfg
}
func ownArchives(t *testing.T, directory, name string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), name+".") && entry.Type().IsRegular() {
			files = append(files, filepath.Join(directory, entry.Name()))
		}
	}
	return files
}

func TestServerRotationRetainsHistoryAndUnrelatedFiles(t *testing.T) {
	cfg := fileLoggingConfig(t)
	logging, err := NewLogging(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 0; i < 4; i++ {
		logging.Logger().Infof("record-%d", i)
		logging.rotate(now.Add(time.Duration(i) * time.Second))
	}
	if err = logging.Close(); err != nil {
		t.Fatal(err)
	}
	files := ownArchives(t, cfg.Directory, "server.log")
	if len(files) != 2 {
		t.Fatalf("archives=%v", files)
	}
	if err = os.WriteFile(filepath.Join(cfg.Directory, "server.log.unrelated"), []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	os.WriteFile(target, []byte("outside"), 0600)
	link := filepath.Join(cfg.Directory, "server.log.20200101T000000.000000000Z.0000000000000000")
	if err = os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	logging, err = NewLogging(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer logging.Close()
	if len(ownArchives(t, cfg.Directory, "server.log")) != 3 {
		t.Fatal("restart discarded history")
	}
	logging.Logger().Infof("after restart")
	logging.rotate(now.Add(10 * time.Second))
	if _, err = os.Lstat(link); err != nil {
		t.Fatalf("unrelated symlink deleted: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(cfg.Directory, "server.log.unrelated"))
	if string(data) != "untouched" {
		t.Fatal("unrelated file modified")
	}
}

func TestServerPeriodRotationUsesPreviousActiveMtimeAndTotalBudget(t *testing.T) {
	cfg := fileLoggingConfig(t)
	cfg.Server.RotationPolicy = &RotationPolicy{RotationPeriodMilliseconds: 1000, MaxTotalSizeToKeep: 8}
	now := time.Now()
	os.WriteFile(cfg.ServerLogPath(), []byte("old log\n"), 0600)
	os.Chtimes(cfg.ServerLogPath(), now.Add(-2*time.Second), now.Add(-2*time.Second))
	logging, err := NewLogging(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer logging.Close()
	logging.rotate(now)
	files := ownArchives(t, cfg.Directory, "server.log")
	if len(files) != 1 {
		t.Fatalf("period did not rotate old file: %v", files)
	}
	logging.Logger().Infof("oversized-record")
	logging.rotate(now.Add(2 * time.Second))
	if len(ownArchives(t, cfg.Directory, "server.log")) != 0 {
		t.Fatal("total archive budget not enforced")
	}
}

func TestConcurrentServerLoggingRotationPreservesEveryRecord(t *testing.T) {
	cfg := fileLoggingConfig(t)
	cfg.Server.RotationPolicy.MaxSegmentCountToKeep = 1000
	logging, err := NewLogging(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var writers sync.WaitGroup
	for g := 0; g < 4; g++ {
		writers.Add(1)
		go func(g int) {
			defer writers.Done()
			for i := 0; i < 100; i++ {
				logging.Logger().Infof("record:%d:%d", g, i)
			}
		}(g)
	}
	for i := 0; i < 40; i++ {
		logging.rotate(time.Now())
	}
	writers.Wait()
	logging.Close()
	files := append(ownArchives(t, cfg.Directory, "server.log"), cfg.ServerLogPath())
	var all strings.Builder
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(data)
	}
	for g := 0; g < 4; g++ {
		for i := 0; i < 100; i++ {
			needle := fmt.Sprintf("record:%d:%d\n", g, i)
			if strings.Count(all.String(), needle) != 1 {
				t.Fatalf("record lost or duplicated: %q", needle)
			}
		}
	}
}

func TestExternalRotationFailureRetryAndServerRestart(t *testing.T) {
	cfg := DefaultLoggingConfig()
	cfg.Directory = t.TempDir()
	cfg.Proxy.WriterType = "file"
	cfg.Proxy.RotationPolicy = &RotationPolicy{MaxSegmentSize: 1, MaxSegmentCountToKeep: 1}
	logging, err := NewLogging(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(cfg.ProxyLogPath()); !os.IsNotExist(err) {
		t.Fatal("server precreated Envoy active file")
	}
	external, err := os.OpenFile(cfg.ProxyLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer external.Close()
	external.WriteString("before\n")
	logging.reopen = func() error { return fmt.Errorf("Envoy unavailable") }
	now := time.Now()
	logging.rotate(now)
	files := ownArchives(t, cfg.Directory, "envoy.log")
	if len(files) != 1 {
		t.Fatalf("archives=%v", files)
	}
	external.WriteString("still writing\n")
	logging.rotate(now.Add(time.Second))
	if len(ownArchives(t, cfg.Directory, "envoy.log")) != 1 {
		t.Fatal("re-rotated pending external file")
	}
	logging.Close()
	logging, err = NewLogging(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer logging.Close()
	logging.reopen = func() error { return nil }
	// Envoy can acknowledge reopen before a quiet stream creates its new file.
	// Waiting must preserve history without reporting a failure on every tick.
	diagnostics, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostics.Close()
	func() {
		previous := os.Stderr
		os.Stderr = diagnostics
		defer func() { os.Stderr = previous }()
		logging.rotate(now.Add(2 * time.Second))
		logging.rotate(now.Add(2500 * time.Millisecond))
	}()
	if info, err := diagnostics.Stat(); err != nil || info.Size() != 0 {
		t.Fatalf("quiet pending reopen emitted diagnostics: info=%v err=%v", info, err)
	}
	if _, err = os.Stat(files[0]); err != nil {
		t.Fatal("successful POST without recreated file deleted archive")
	}
	logging.reopen = func() error { return os.WriteFile(cfg.ProxyLogPath(), nil, 0600) }
	logging.rotate(now.Add(3 * time.Second))
	data, _ := os.ReadFile(files[0])
	if !strings.Contains(string(data), "still writing") {
		t.Fatal("old open inode data lost")
	}
	logging.rotate(now.Add(4 * time.Second))
	if _, err = os.Stat(files[0]); err != nil {
		t.Fatal("latest external archive pruned before subsequent reopen")
	}
	os.WriteFile(cfg.ProxyLogPath(), []byte("next\n"), 0600)
	logging.rotate(now.Add(5 * time.Second))
	if len(ownArchives(t, cfg.Directory, "envoy.log")) != 1 {
		t.Fatal("old protected archive not released on subsequent reopen")
	}
	if _, err = os.Stat(files[0]); !os.IsNotExist(err) {
		t.Fatal("obsolete protected file retained")
	}
}

func TestExternalPendingAndPreviousProtectedArchiveSurviveRetry(t *testing.T) {
	cfg := DefaultLoggingConfig()
	cfg.Directory = t.TempDir()
	cfg.Proxy.WriterType = "file"
	cfg.Proxy.RotationPolicy = &RotationPolicy{MaxSegmentSize: 1, MaxSegmentCountToKeep: 1}
	logging, err := NewLogging(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer logging.Close()
	logging.reopen = func() error { return os.WriteFile(cfg.ProxyLogPath(), nil, 0600) }
	os.WriteFile(cfg.ProxyLogPath(), []byte("first"), 0600)
	logging.rotate(time.Now())
	first := ownArchives(t, cfg.Directory, "envoy.log")[0]
	logging.reopen = func() error { return fmt.Errorf("not reopened") }
	os.WriteFile(cfg.ProxyLogPath(), []byte("second"), 0600)
	logging.rotate(time.Now())
	if len(ownArchives(t, cfg.Directory, "envoy.log")) != 2 {
		t.Fatal("pending rotation pruned previous protected archive")
	}
	if _, err = os.Stat(first); err != nil {
		t.Fatal(err)
	}
}

func TestLoggingRejectsSymlinkActive(t *testing.T) {
	cfg := fileLoggingConfig(t)
	target := filepath.Join(t.TempDir(), "target")
	os.WriteFile(target, []byte("keep"), 0600)
	os.Symlink(target, cfg.ServerLogPath())
	if logging, err := NewLogging(cfg); err == nil {
		logging.Close()
		t.Fatal("accepted symlink active file")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "keep" {
		t.Fatal("symlink target changed")
	}
}

func TestServerLoggingContinuesDuringExternalReopen(t *testing.T) {
	cfg := fileLoggingConfig(t)
	cfg.Proxy.WriterType = "file"
	cfg.Proxy.RotationPolicy = &RotationPolicy{MaxSegmentSize: 1, MaxSegmentCountToKeep: 1}
	logging, err := NewLogging(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer logging.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	logging.reopen = func() error { close(entered); <-release; return os.WriteFile(cfg.ProxyLogPath(), nil, 0600) }
	os.WriteFile(cfg.ProxyLogPath(), []byte("external"), 0600)
	rotated := make(chan struct{})
	go func() { logging.rotate(time.Now()); close(rotated) }()
	<-entered
	written := make(chan struct{})
	go func() { logging.Logger().Infof("during reopen"); close(written) }()
	select {
	case <-written:
	case <-time.After(time.Second):
		close(release)
		<-rotated
		t.Fatal("server logging blocked by external reopen")
	}
	close(release)
	<-rotated
	data, err := os.ReadFile(cfg.ServerLogPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "during reopen") {
		t.Fatal("record missing")
	}
}

func TestExternalReopenRejectsSymlinkAndOriginalInode(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			cfg := DefaultLoggingConfig()
			cfg.Directory = t.TempDir()
			cfg.Proxy.WriterType = "file"
			cfg.Proxy.RotationPolicy = &RotationPolicy{MaxSegmentSize: 1, MaxTotalSizeToKeep: 1}
			logging, err := NewLogging(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer logging.Close()
			os.WriteFile(cfg.ProxyLogPath(), []byte("protected original data"), 0600)
			logging.reopen = func() error {
				files := ownArchives(t, cfg.Directory, "envoy.log")
				if kind == "symlink" {
					return os.Symlink(files[0], cfg.ProxyLogPath())
				}
				return os.Link(files[0], cfg.ProxyLogPath())
			}
			logging.rotate(time.Now())
			files := ownArchives(t, cfg.Directory, "envoy.log")
			if len(files) != 1 {
				t.Fatal("archive pruned despite invalid recreation")
			}
			if logging.streams[0].pending == "" {
				t.Fatal("same inode or symlink accepted as recreated active")
			}
		})
	}
}

func TestExternalArchiveTotalBudgetReleasesPriorProtectedSegment(t *testing.T) {
	cfg := DefaultLoggingConfig()
	cfg.Directory = t.TempDir()
	cfg.Proxy.WriterType = "file"
	cfg.Proxy.RotationPolicy = &RotationPolicy{MaxSegmentSize: 1, MaxTotalSizeToKeep: 3}
	logging, err := NewLogging(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer logging.Close()
	logging.reopen = func() error { return os.WriteFile(cfg.ProxyLogPath(), nil, 0600) }
	os.WriteFile(cfg.ProxyLogPath(), []byte("oversized buffered archive"), 0600)
	now := time.Now()
	logging.rotate(now)
	first := ownArchives(t, cfg.Directory, "envoy.log")
	if len(first) != 1 {
		t.Fatal("protected latest archive deleted to satisfy byte quota")
	}
	os.WriteFile(cfg.ProxyLogPath(), []byte("new"), 0600)
	logging.rotate(now.Add(time.Second))
	files := ownArchives(t, cfg.Directory, "envoy.log")
	if len(files) != 1 {
		t.Fatalf("archives=%v", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("wrong retained data: %s", data)
	}
}

func TestExternalRestartIgnoresEmptyArchiveReservation(t *testing.T) {
	cfg := DefaultLoggingConfig()
	cfg.Directory = t.TempDir()
	cfg.Proxy.WriterType = "file"
	cfg.Proxy.RotationPolicy = &RotationPolicy{RotationPeriodMilliseconds: 3600000, MaxSegmentCountToKeep: 1}
	now := time.Now()
	prior := cfg.ProxyLogPath() + "." + now.Add(-2*time.Hour).UTC().Format("20060102T150405.000000000Z") + ".1111111111111111"
	held, err := os.OpenFile(prior, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err = held.WriteString("prior real archive\n"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cfg.ProxyLogPath(), []byte("recent active record\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Persistent state of a crash after old-style reservation but before active rename.
	placeholder := cfg.ProxyLogPath() + "." + now.UTC().Format("20060102T150405.000000000Z") + ".2222222222222222"
	if err = os.WriteFile(placeholder, nil, 0600); err != nil {
		t.Fatal(err)
	}
	logging, err := NewLogging(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer logging.Close()
	logging.reopen = func() error { t.Error("recent active should not require reopen"); return nil }
	logging.rotate(now)
	if _, err = os.Stat(prior); err != nil {
		t.Fatalf("real protected archive lost after reservation crash: %v", err)
	}
	if _, err = held.WriteString("late buffered record\n"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(prior)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "late buffered record") {
		t.Fatal("late Envoy record lost")
	}
	if _, err = os.Stat(placeholder); !os.IsNotExist(err) {
		t.Fatal("empty unprotected placeholder retained over count quota")
	}
}

func TestRetentionAccountsForOneConsistentSnapshot(t *testing.T) {
	directory := t.TempDir()
	first, second, last := filepath.Join(directory, "first"), filepath.Join(directory, "second"), filepath.Join(directory, "last")
	for path, size := range map[string]int{first: 40, second: 30, last: 50} {
		if err := os.WriteFile(path, make([]byte, size), 0600); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := []logArchive{{path: first, size: 40}, {path: second, size: 30}, {path: last, size: 50}}
	// External buffered writes occur after the directory-size scan.
	if err := os.WriteFile(first, make([]byte, 1000), 0600); err != nil {
		t.Fatal(err)
	}
	stream := &rotatingLog{policy: RotationPolicy{MaxTotalSizeToKeep: 100}, protected: last}
	if err := stream.pruneArchives(snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatal("oldest archive not removed")
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("archive below remaining 80-byte snapshot budget removed: %v", err)
	}
	if _, err := os.Stat(last); err != nil {
		t.Fatal(err)
	}
}

func TestExternalArchiveChronologySurvivesBackwardClockAndRestart(t *testing.T) {
	cfg := DefaultLoggingConfig()
	cfg.Directory = t.TempDir()
	cfg.Proxy.WriterType = "file"
	cfg.Proxy.RotationPolicy = &RotationPolicy{MaxSegmentSize: 1, MaxSegmentCountToKeep: 3}
	logging, err := NewLogging(cfg)
	if err != nil {
		t.Fatal(err)
	}
	logging.reopen = func() error { return os.WriteFile(cfg.ProxyLogPath(), nil, 0600) }
	now := time.Now()
	if err = os.WriteFile(cfg.ProxyLogPath(), []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	logging.rotate(now)
	if err = os.WriteFile(cfg.ProxyLogPath(), []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	logging.rotate(now.Add(-time.Hour))
	logging.Close()
	logging, err = NewLogging(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer logging.Close()
	if logging.streams[0].protected == "" {
		t.Fatal("missing restart protection")
	}
	data, err := os.ReadFile(logging.streams[0].protected)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("wrong latest protected archive after backward clock: %q", data)
	}
	logging.reopen = func() error { return os.WriteFile(cfg.ProxyLogPath(), nil, 0600) }
	if err = os.WriteFile(cfg.ProxyLogPath(), []byte("third"), 0600); err != nil {
		t.Fatal(err)
	}
	logging.rotate(now.Add(-2 * time.Hour))
	files := ownArchives(t, cfg.Directory, "envoy.log")
	data, err = os.ReadFile(files[len(files)-1])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "third" {
		t.Fatalf("restart rotation did not preserve archive chronology: %q", data)
	}
}
