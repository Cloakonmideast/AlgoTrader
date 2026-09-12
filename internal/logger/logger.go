// Package logger configures the standard library logger to emit structured JSON
// records to a log file while preserving the original plain-text output on stdout.
//
// Usage:
//
//	if err := logger.Init("logs/algotrader.log"); err != nil {
//	    log.Fatalf("logger: %v", err)
//	}
//
// After Init, every call to log.Printf / log.Println anywhere in the program
// writes:
//   - Plain text  → stdout (unchanged behaviour)
//   - JSON record → the configured log file, one object per line
//
// JSON record shape:
//
//	{"time":"2026-06-12T01:30:00+05:30","level":"INFO","msg":"[processor] ..."}
//
// Level is inferred from the message text:
//   - contains "ERROR" or "FATAL" → "ERROR"
//   - contains "WARN"             → "WARN"
//   - otherwise                   → "INFO"
package logger

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// jsonWriter is an io.Writer that converts each plain-text log line produced by
// the standard logger into a JSON object and writes it to an underlying file.
// It is safe for concurrent use.
type jsonWriter struct {
	mu   sync.Mutex
	file *os.File
}

// globalWriter holds the active jsonWriter so Close() can flush and close the file.
var globalWriter *jsonWriter

// logRecord is the JSON schema for each log line.
type logRecord struct {
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// Write implements io.Writer. Each call receives exactly one log line (the
// standard library always calls Write once per log entry). The line is trimmed
// and wrapped in a logRecord before being encoded as a JSON object.
func (w *jsonWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimRight(string(p), "\n\r")

	// Strip the standard log prefix (date + time + microseconds + file:line)
	// so the "msg" field contains only the application message.
	// The stdlib prefix looks like: "2026/06/12 01:30:00.000000 main.go:42: "
	// We detect it by looking for the last ": " after position 20.
	if idx := strings.Index(msg, ": "); idx > 0 && idx < 60 {
		msg = msg[idx+2:]
	}

	level := inferLevel(msg)

	rec := logRecord{
		Time:  time.Now().Format(time.RFC3339),
		Level: level,
		Msg:   msg,
	}

	b, encErr := json.Marshal(rec)
	if encErr != nil {
		// If marshalling fails for any reason, write a raw fallback record.
		b = []byte(`{"level":"ERROR","msg":"logger: json marshal failed"}`)
	}
	b = append(b, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.file.Write(b)
	// Always report that we consumed all input bytes so the logger does not
	// treat a short write as an error and abort.
	return len(p), err
}

// inferLevel returns the log level string based on keywords in the message.
func inferLevel(msg string) string {
	upper := strings.ToUpper(msg)
	switch {
	case strings.Contains(upper, "FATAL") || strings.Contains(upper, "ERROR"):
		return "ERROR"
	case strings.Contains(upper, "WARN"):
		return "WARN"
	default:
		return "INFO"
	}
}

// Init configures the global standard logger to fan-out to both stdout (plain
// text) and a structured JSON log file at logFilePath.
//
// The directory for logFilePath is created automatically if it does not exist.
// Init must be called once, before any goroutines that call log.Printf are started.
func Init(logFilePath string) error {
	// Ensure parent directory exists.
	dir := filepath.Dir(logFilePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// Open (or create) the log file in append mode so restarts do not truncate history.
	f, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}

	jw := &jsonWriter{file: f}
	globalWriter = jw

	// Fan-out: plain text to stdout, JSON records to file.
	multi := io.MultiWriter(os.Stdout, jw)
	log.SetOutput(multi)

	log.Printf("[logger] Structured JSON logging initialised → %s", logFilePath)
	return nil
}

// Close flushes any buffered data and closes the underlying log file.
// It should be called once during application shutdown after all goroutines that
// produce log output have stopped. Subsequent log lines will still appear on
// stdout but will no longer be written to the JSON file.
func Close() error {
	if globalWriter == nil {
		return nil
	}
	globalWriter.mu.Lock()
	defer globalWriter.mu.Unlock()
	err := globalWriter.file.Close()
	globalWriter = nil
	return err
}
