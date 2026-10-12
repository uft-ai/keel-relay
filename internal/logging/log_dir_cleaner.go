package logging

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const logDirCleanerInterval = time.Minute

var logDirCleanerCancel context.CancelFunc

var activeRequestLogs = struct {
	sync.Mutex
	paths map[string]struct{}
}{paths: make(map[string]struct{})}

// Opening and deletion share a lock so a cleaner cannot remove a log before protection is registered.
type requestLogFile struct{ *os.File }

func openRequestLogFile(path string) (*requestLogFile, error) {
	activeRequestLogs.Lock()
	defer activeRequestLogs.Unlock()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	activeRequestLogs.paths[requestLogPath(path)] = struct{}{}
	return &requestLogFile{File: file}, nil
}

func (file *requestLogFile) Close() error {
	activeRequestLogs.Lock()
	defer activeRequestLogs.Unlock()
	err := file.File.Close()
	delete(activeRequestLogs.paths, requestLogPath(file.Name()))
	return err
}

func removeInactiveLog(path string) (bool, error) {
	activeRequestLogs.Lock()
	defer activeRequestLogs.Unlock()
	if _, active := activeRequestLogs.paths[requestLogPath(path)]; active {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
}

var completedRequestLogName = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}T\d{6}(?:_\d+)?-[A-Za-z0-9_-]+\.log(?:\.gz)?$`)

var orphanSpoolName = regexp.MustCompile(`^(?:(?:request|response)-body-[0-9]+\.tmp|request-log-parts-(?:request-body|api-request|api-response|websocket-timeline|api-websocket-timeline)-[0-9]+)$`)

// IsRequestLogActive distinguishes a completed capture from a file still being assembled.
func IsRequestLogActive(path string) bool {
	activeRequestLogs.Lock()
	defer activeRequestLogs.Unlock()
	_, active := activeRequestLogs.paths[requestLogPath(path)]
	return active
}

func requestLogPath(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		return absolute
	}
	return filepath.Clean(path)
}

// CleanupOrphanSpools runs before serving requests in a private single-instance log directory.
// The process-start cutoff leaves every spool created by this process untouched.
func (logger *FileRequestLogger) CleanupOrphanSpools(processStarted time.Time) (int, error) {
	entries, err := os.ReadDir(logger.logsDir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		if !orphanSpoolName.MatchString(entry.Name()) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(processStarted) {
			continue
		}
		if strings.HasPrefix(entry.Name(), "request-log-parts-") != info.IsDir() || (!info.IsDir() && !info.Mode().IsRegular()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(logger.logsDir, entry.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func configureLogDirCleanerLocked(logDir string, maxTotalSizeMB, maxAgeHours int, protectedPath string) {
	stopLogDirCleanerLocked()

	if maxTotalSizeMB <= 0 && maxAgeHours <= 0 {
		return
	}

	maxBytes := int64(maxTotalSizeMB) * 1024 * 1024

	dir := strings.TrimSpace(logDir)
	if dir == "" {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	logDirCleanerCancel = cancel
	go runLogDirCleaner(ctx, filepath.Clean(dir), maxBytes, time.Duration(maxAgeHours)*time.Hour, strings.TrimSpace(protectedPath))
}

func stopLogDirCleanerLocked() {
	if logDirCleanerCancel == nil {
		return
	}
	logDirCleanerCancel()
	logDirCleanerCancel = nil
}

func runLogDirCleaner(ctx context.Context, logDir string, maxBytes int64, maxAge time.Duration, protectedPath string) {
	ticker := time.NewTicker(logDirCleanerInterval)
	defer ticker.Stop()

	cleanOnce := func() {
		if _, err := enforceRequestLogMaxAge(logDir, maxAge, protectedPath, time.Now()); err != nil {
			log.WithError(err).Warn("logging: failed to expire request captures")
		}
		deleted, errClean := enforceLogDirSizeLimit(logDir, maxBytes, protectedPath)
		if errClean != nil {
			log.WithError(errClean).Warn("logging: failed to enforce log directory size limit")
			return
		}
		if deleted > 0 {
			log.Debugf("logging: removed %d old log file(s) to enforce log directory size limit", deleted)
		}
	}

	cleanOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanOnce()
		}
	}
}

func enforceLogDirSizeLimit(logDir string, maxBytes int64, protectedPath string) (int, error) {
	if maxBytes <= 0 {
		return 0, nil
	}

	dir := strings.TrimSpace(logDir)
	if dir == "" {
		return 0, nil
	}
	dir = filepath.Clean(dir)

	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return 0, nil
		}
		return 0, errRead
	}

	protected := strings.TrimSpace(protectedPath)
	if protected != "" {
		protected = filepath.Clean(protected)
	}

	type logFile struct {
		path    string
		size    int64
		modTime time.Time
	}

	var (
		files []logFile
		total int64
	)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !isLogFileName(name) {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(dir, name)
		files = append(files, logFile{
			path:    path,
			size:    info.Size(),
			modTime: info.ModTime(),
		})
		total += info.Size()
	}

	if total <= maxBytes {
		return 0, nil
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	deleted := 0
	for _, file := range files {
		if total <= maxBytes {
			break
		}
		if protected != "" && filepath.Clean(file.path) == protected {
			continue
		}
		removed, errRemove := removeInactiveLog(file.path)
		if errRemove != nil {
			log.WithError(errRemove).Warnf("logging: failed to remove old log file: %s", filepath.Base(file.path))
			continue
		}
		if !removed {
			continue
		}
		total -= file.size
		deleted++
	}

	return deleted, nil
}

func enforceRequestLogMaxAge(dir string, maxAge time.Duration, protectedPath string, now time.Time) (int, error) {
	if maxAge <= 0 || strings.TrimSpace(dir) == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !completedRequestLogName.MatchString(name) {
			continue
		}
		path := filepath.Join(dir, name)
		if protectedPath != "" && filepath.Clean(path) == filepath.Clean(protectedPath) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().After(now.Add(-maxAge)) {
			continue
		}
		removed, err := removeInactiveLog(path)
		if err != nil && !os.IsNotExist(err) {
			return deleted, err
		}
		if removed {
			deleted++
		}
	}
	return deleted, nil
}

func isLogFileName(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	return strings.HasSuffix(lower, ".log") || strings.HasSuffix(lower, ".log.gz")
}
