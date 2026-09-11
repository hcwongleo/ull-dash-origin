package server

import (
	"log"
	"strings"
	"sync/atomic"
)

// Log levels. Upstream logged three lines per request unconditionally — the
// request line, plus "NEW File Content-Type" per PUT and "Reading from memory"
// per GET — which is both a throughput cost and the thing that fills journald.
//
// Kept deliberately small: a level, and no sampling. Sampling was considered and
// dropped as complexity that buys nothing once per-request logging is off by
// default.
const (
	LevelError int32 = iota
	LevelWarn
	LevelInfo
	LevelDebug
)

var logLevel atomic.Int32

// SetLogLevel parses a level name. Unknown values fall back to warn, which is the
// level at which the server reports only things an operator should act on.
func SetLogLevel(name string) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "error":
		logLevel.Store(LevelError)
	case "info":
		logLevel.Store(LevelInfo)
	case "debug":
		logLevel.Store(LevelDebug)
	default:
		logLevel.Store(LevelWarn)
	}
}

func enabled(l int32) bool { return logLevel.Load() >= l }

func logErrorf(format string, v ...interface{}) {
	log.Printf("ERROR "+format, v...)
}

func logWarnf(format string, v ...interface{}) {
	if enabled(LevelWarn) {
		log.Printf("WARN "+format, v...)
	}
}

func logInfof(format string, v ...interface{}) {
	if enabled(LevelInfo) {
		log.Printf("INFO "+format, v...)
	}
}

func logDebugf(format string, v ...interface{}) {
	if enabled(LevelDebug) {
		log.Printf("DEBUG "+format, v...)
	}
}
