// Package logger provides leveled logging for icloud-reminders.
//
// Levels:
//
//	0 (default) — silent; only command output is shown
//	1 (-v)      — info:  session status, sync summary, write operations
//	2 (-vv)     — debug: HTTP requests, credential source, record counts, timing
package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"time"
)

var level int

var (
	infoLog  = log.New(io.Discard, "", 0)
	debugLog = log.New(io.Discard, "", 0)
)

// SetLevel configures the active log level (0=silent, 1=info, 2=debug).
func SetLevel(v int) {
	level = v
	infoLog = log.New(io.Discard, "", 0)
	debugLog = log.New(io.Discard, "", 0)
	if v >= 1 {
		infoLog = log.New(os.Stderr, "", 0)
	}
	if v >= 2 {
		debugLog = log.New(os.Stderr, "[debug] ", 0)
	}
}

// Level returns the current verbosity level.
func Level() int { return level }

// Info logs at info level (visible with -v).
func Info(msg string) { infoLog.Print(msg) }

// Infof logs a formatted message at info level.
func Infof(format string, args ...interface{}) { infoLog.Printf(format, args...) }

// Debug logs at debug level (visible with -vv).
func Debug(msg string) { debugLog.Print(msg) }

// Debugf logs a formatted message at debug level.
func Debugf(format string, args ...interface{}) { debugLog.Printf(format, args...) }

// Timer returns a function that logs elapsed time at debug level when called.
//
//	defer logger.Timer("sync")()
func Timer(name string) func() {
	if level < 2 {
		return func() {}
	}
	start := time.Now()
	return func() {
		debugLog.Printf("%s took %s", name, time.Since(start).Round(time.Millisecond))
	}
}

// Warn always prints to stderr regardless of level.
func Warn(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "⚠️  "+format+"\n", args...)
}
