// logger used throughout the application in place of the standard library
// "log/slog" package. This avoids a hard requirement on Go 1.21+ at build
// time (log/slog was introduced in Go 1.21), allowing the binary to build
// on older toolchains that may be present in some execution environments,
// while still providing leveled, key/value structured logging suitable for
// container log aggregation (Cloud Logging, etc.).
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Level represents a logging severity level.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

func parseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

// Logger is a minimal leveled, structured logger. It is safe for
// concurrent use. Output is line-oriented text in the form:
//
//	2006-01-02T15:04:05.000Z LEVEL msg key=value key2=value2
//
// This intentionally mirrors the ergonomics of "log/slog" (level methods
// taking a message plus alternating key/value pairs) without depending on
// that package, so call sites elsewhere in the codebase are unaffected.
type Logger struct {
	mu     sync.Mutex
	out    io.Writer
	level  Level
	fields []any // base key/value pairs attached via With
}

// NewLogger constructs a Logger writing to w, filtering out entries below
// minLevel (parsed via ParseLevel-style strings: "debug","info","warn","error").
func NewLogger(w io.Writer, levelStr string) *Logger {
	if w == nil {
		w = os.Stdout
	}
	return &Logger{out: w, level: parseLevel(levelStr)}
}

// With returns a new Logger that always includes the given key/value pairs.
func (l *Logger) With(kv ...any) *Logger {
	if l == nil {
		return nil
	}
	nl := &Logger{out: l.out, level: l.level}
	nl.fields = append(append([]any{}, l.fields...), kv...)
	return nl
}

func (l *Logger) log(level Level, msg string, kv []any) {
	if l == nil {
		return
	}
	if level < l.level {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	var sb strings.Builder
	sb.WriteString(time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	sb.WriteByte(' ')
	sb.WriteString(level.String())
	sb.WriteByte(' ')
	sb.WriteString(msg)

	writePairs := func(pairs []any) {
		for i := 0; i+1 < len(pairs); i += 2 {
			k := fmt.Sprintf("%v", pairs[i])
			v := pairs[i+1]
			sb.WriteByte(' ')
			sb.WriteString(k)
			sb.WriteByte('=')
			fmt.Fprintf(&sb, "%q", fmt.Sprintf("%v", v))
		}
	}
	writePairs(l.fields)
	writePairs(kv)
	sb.WriteByte('\n')

	_, _ = io.WriteString(l.out, sb.String())
}

// Debug logs at debug level with alternating key/value pairs.
func (l *Logger) Debug(msg string, kv ...any) { l.log(LevelDebug, msg, kv) }

// Info logs at info level with alternating key/value pairs.
func (l *Logger) Info(msg string, kv ...any) { l.log(LevelInfo, msg, kv) }

// Warn logs at warn level with alternating key/value pairs.
func (l *Logger) Warn(msg string, kv ...any) { l.log(LevelWarn, msg, kv) }

// Error logs at error level with alternating key/value pairs.
func (l *Logger) Error(msg string, kv ...any) { l.log(LevelError, msg, kv) }
