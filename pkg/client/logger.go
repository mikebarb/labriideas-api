package client

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// syncWriter wraps an io.Writer with a mutex so concurrent writes from
// multiple goroutines don't interleave.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func newSyncWriter(writers ...io.Writer) *syncWriter {
	return &syncWriter{w: io.MultiWriter(writers...)}
}

// SetupLogging configures the standard log package to write to BOTH the
// terminal (os.Stdout) and a timestamped log file. After this call:
//   - log.Print* / log.Fatal* / log.Panic* all go to both destinations.
//   - fmt.Print* still goes to the terminal only (use Print/Printf below
//     to also capture them in the log file).
//
// If logDir is empty, the current working directory is used.
// If the log file cannot be created, SetupLogging returns an empty path
// and logging continues to the terminal only.
func SetupLogging(logDir string) string {
	if logDir == "" {
		logDir, _ = os.Getwd()
	}

	if err := os.MkdirAll(logDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Could not create log directory %q: %v\n", logDir, err)
		return ""
	}

	ts := time.Now().Format("2006-01-02_15-04-05")
	logPath := filepath.Join(logDir, fmt.Sprintf("labriideas-%s.log", ts))

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  Could not open log file %q: %v\n", logPath, err)
		return ""
	}

	// Wire the log package to a synchronized MultiWriter that fans out to
	// (a) the original terminal and (b) the log file.
	log.SetOutput(newSyncWriter(os.Stdout, f))
	log.SetFlags(log.LstdFlags)

	return logPath
}

// Print is a convenience alias for log.Print that uses the same output
// configuration as SetupLogging. Use this instead of fmt.Println for any
// line you want captured in the log file.
func Print(v ...interface{}) {
	log.Print(v...)
}

// Printf is the formatted equivalent of Print. Use this instead of fmt.Printf
// for any line you want captured in the log file.
func Printf(format string, v ...interface{}) {
	log.Printf(format, v...)
}
