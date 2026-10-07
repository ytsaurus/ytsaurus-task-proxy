package pkg

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"
)

const archiveTimestampLayout = "20060102T150405.000000000Z"

type rotatingLog struct {
	path      string
	policy    RotationPolicy
	external  bool
	file      *os.File
	since     time.Time
	pending   string
	protected string
}
type logArchive struct {
	path string
	size int64
}

func regularLogInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("log path is not a regular file: %s", path)
	}
	return info, nil
}
func openLogFile(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_APPEND|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0640)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("log path is not a regular file: %s", path)
	}
	return file, nil
}
func (s *rotatingLog) archives() ([]logArchive, error) {
	directory := filepath.Dir(s.path)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	own := regexp.MustCompile(`^` + regexp.QuoteMeta(filepath.Base(s.path)) + `\.[0-9]{8}T[0-9]{6}\.[0-9]{9}Z\.[0-9a-f]{16}$`)
	var result []logArchive
	for _, entry := range entries {
		if !own.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		result = append(result, logArchive{filepath.Join(directory, entry.Name()), info.Size()})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].path < result[j].path })
	return result, nil
}
func (s *rotatingLog) rename(now time.Time) (string, error) {
	archives, err := s.archives()
	if err != nil {
		return "", err
	}
	if len(archives) > 0 {
		latest := archives[len(archives)-1].path
		offset := len(s.path) + 1
		previous, err := time.Parse(archiveTimestampLayout, latest[offset:offset+len(archiveTimestampLayout)])
		if err != nil {
			return "", fmt.Errorf("invalid archive timestamp %s: %w", latest, err)
		}
		// Keep filename chronology monotonic, including after restart and
		// backwards wall-clock adjustments. Retention/recovery use this order.
		if !now.After(previous) {
			now = previous.Add(time.Nanosecond)
		}
	}
	var suffix [8]byte
	for attempts := 0; attempts < 10; attempts++ {
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", err
		}
		archive := s.path + "." + now.UTC().Format(archiveTimestampLayout) + "." + hex.EncodeToString(suffix[:])
		// Reserve a collision-free name before rename (never overwrite an older archive).
		reserved, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		reserved.Close()
		if err = os.Rename(s.path, archive); err != nil {
			os.Remove(archive)
			return "", err
		}
		return archive, nil
	}
	return "", fmt.Errorf("could not reserve log archive name")
}
func (s *rotatingLog) due(now time.Time) (bool, error) {
	info, err := regularLogInfo(s.path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Size() == 0 {
		return false, nil
	}
	return s.policy.MaxSegmentSize > 0 && info.Size() >= int64(s.policy.MaxSegmentSize) || s.policy.RotationPeriodMilliseconds > 0 && now.Sub(s.since) >= time.Duration(s.policy.RotationPeriodMilliseconds)*time.Millisecond, nil
}
func (s *rotatingLog) prune() error {
	archives, err := s.archives()
	if err != nil {
		return err
	}
	return s.pruneArchives(archives)
}

func (s *rotatingLog) pruneArchives(archives []logArchive) error {
	// Protected files count against the budget, but cannot be deleted until a later reopen.
	var total uint64
	for _, archive := range archives {
		total += uint64(archive.size)
	}
	count := int64(len(archives))
	for _, archive := range archives {
		over := s.policy.MaxSegmentCountToKeep > 0 && count > s.policy.MaxSegmentCountToKeep || s.policy.MaxTotalSizeToKeep > 0 && total > uint64(s.policy.MaxTotalSizeToKeep)
		if !over {
			break
		}
		if archive.path == s.protected || archive.path == s.pending {
			continue
		}
		// Recheck the path: do not remove a symlink swapped into an archive name.
		_, err := regularLogInfo(archive.path)
		if err != nil {
			return err
		}
		if err = os.Remove(archive.path); err != nil {
			return err
		}
		count--
		// External buffered writes may change the size after the scan.
		// Subtract the same snapshot used to accumulate total; recheck next tick.
		total -= uint64(archive.size)
	}
	return nil
}

func (l *Logging) rotate(now time.Time) {
	l.rotationMu.Lock()
	defer l.rotationMu.Unlock()
	if l.closed {
		return
	}
	for _, stream := range l.streams {
		if stream.pending != "" {
			continue
		}
		if !stream.external {
			l.state.mu.Lock()
		}
		due, err := stream.due(now)
		if err == nil && due {
			stream.pending, err = stream.rename(now)
		}
		if !stream.external {
			if err == nil && stream.pending != "" {
				err = l.reopenServer(stream, now)
			}
			l.state.mu.Unlock()
		}
		if err != nil {
			loggingError("rotate %s: %v", stream.path, err)
		}
	}
	// Retry server open failures while continuing to write through the old descriptor.
	for _, stream := range l.streams {
		if !stream.external && stream.pending != "" {
			l.state.mu.Lock()
			err := l.reopenServer(stream, now)
			l.state.mu.Unlock()
			if err != nil {
				loggingError("reopen %s: %v", stream.path, err)
			}
		}
	}
	pending := false
	for _, stream := range l.streams {
		if stream.external && stream.pending != "" {
			pending = true
		}
	}
	if pending {
		// Never hold the server logging mutex during the network call.
		err := l.reopen()
		if err != nil {
			loggingError("reopen Envoy logs: %v", err)
		} else {
			for _, stream := range l.streams {
				if !stream.external || stream.pending == "" {
					continue
				}
				active, err := regularLogInfo(stream.path)
				archived, archiveErr := regularLogInfo(stream.pending)
				if os.IsNotExist(err) && archiveErr == nil {
					// Envoy recreates quiet streams lazily. Keep waiting and
					// protecting the archive without reporting a false failure.
					continue
				}
				if err != nil || archiveErr != nil || os.SameFile(active, archived) {
					loggingError("Envoy did not recreate %s after reopen", stream.path)
					continue
				}
				stream.protected = stream.pending
				stream.pending = ""
				stream.since = now
			}
		}
	}
	for _, stream := range l.streams {
		if stream.pending != "" {
			continue
		}
		if err := stream.prune(); err != nil {
			loggingError("prune %s: %v", stream.path, err)
		}
	}
}
func (l *Logging) reopenServer(stream *rotatingLog, now time.Time) error {
	file, err := openLogFile(stream.path)
	if err != nil {
		return err
	}
	previous := stream.file
	stream.file = file
	l.state.writer = file
	stream.pending = ""
	stream.since = now
	if previous != nil {
		return previous.Close()
	}
	return nil
}
