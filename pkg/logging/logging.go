// Package logging adds levels to the standard log package without changing
// how existing lines look.
//
// One LOG_LEVEL environment variable (debug, info, warn, error; default info)
// filters the Debugf/Infof/Warnf/Errorf helpers. The helpers write through the
// standard logger (log.Output), so they share its writer, flags and prefix,
// read at each call:
//
//	2026/11/19 10:00:00 kubernetes: cluster prod connected          (info)
//	2026/11/19 10:00:00 WARN kubernetes: cluster prod forbidden ... (warn)
//	2026/11/19 10:00:00 ERROR storage: postgres unavailable ...     (error)
//	2026/11/19 10:00:00 DEBUG kubernetes: inventory kind=Pod ...    (debug)
//
// An info line is byte-for-byte what log.Printf prints. Existing log.Printf
// calls are not touched: the package never replaces the standard logger or
// the slog default, so a log.Printf line looks the same before and after
// Setup.
//
// The helpers do not redact. Callers never pass secrets (tokens, keys,
// passwords, Authorization headers, URLs with credentials) or payload bodies,
// and run error text through the redaction helper before logging it.
package logging

import (
	"fmt"
	"log"
	"log/slog"
	"strings"
)

// level is the process-wide minimum level. Its zero value is info.
var level slog.LevelVar

// ParseLevel maps a LOG_LEVEL value to a level. It is case-insensitive and
// accepts debug, info, warn or warning, and error. Empty and unknown values
// resolve to info.
func ParseLevel(s string) slog.Level {
	lvl, _ := parseLevel(s)
	return lvl
}

// parseLevel is ParseLevel plus whether the value was recognised. Empty counts
// as recognised: it is the documented way to ask for the default.
func parseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return slog.LevelInfo, true
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// Setup sets the package level from a LOG_LEVEL value and returns it. An
// unknown value logs one warning naming it and falls back to info, so a typo
// never hides warnings. Setup does not touch the standard logger or the slog
// default; only the helpers in this package are filtered.
func Setup(value string) slog.Level {
	lvl, ok := parseLevel(value)
	level.Set(lvl)
	if !ok {
		emit(slog.LevelWarn, 2, fmt.Sprintf("logging: LOG_LEVEL=%q is not a level; using info. Set LOG_LEVEL to debug, info, warn or error", value))
	}
	return lvl
}

// Level returns the current package level.
func Level() slog.Level { return level.Level() }

// LevelName returns the lower-case LOG_LEVEL spelling of lvl: debug, info,
// warn or error.
func LevelName(lvl slog.Level) string {
	switch {
	case lvl < slog.LevelInfo:
		return "debug"
	case lvl < slog.LevelWarn:
		return "info"
	case lvl < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
}

// Enabled reports whether a line at lvl would be written. Use it to skip
// building debug details that are costly to compute.
func Enabled(lvl slog.Level) bool { return lvl >= level.Level() }

// Debugf logs detail for troubleshooting: progress, timings, counts, success
// and recovery lines, retries expected to heal. Hidden unless LOG_LEVEL=debug.
func Debugf(format string, args ...any) { logf(slog.LevelDebug, format, args...) }

// Infof logs a line a user must see: one startup line per enabled feature, or
// a state change. The line is identical to log.Printf output.
func Infof(format string, args ...any) { logf(slog.LevelInfo, format, args...) }

// Warnf logs a problem the user can act on. Name what failed, the cause and
// what to check.
func Warnf(format string, args ...any) { logf(slog.LevelWarn, format, args...) }

// Errorf logs a failure that will not heal on its own. Name what failed, the
// cause and what to check.
func Errorf(format string, args ...any) { logf(slog.LevelError, format, args...) }

func logf(lvl slog.Level, format string, args ...any) {
	if !Enabled(lvl) {
		return
	}
	// Depth 3: emit -> logf -> the exported helper -> its caller, so
	// log.Lshortfile names the caller's file.
	emit(lvl, 3, fmt.Sprintf(format, args...))
}

// emit writes msg through the standard logger with the level tag after the
// timestamp. depth counts the frames between emit and the line to report for
// log.Lshortfile/Llongfile.
func emit(lvl slog.Level, depth int, msg string) {
	_ = log.Output(depth+1, tag(lvl)+msg)
}

func tag(lvl slog.Level) string {
	switch {
	case lvl < slog.LevelInfo:
		return "DEBUG "
	case lvl < slog.LevelWarn:
		return ""
	case lvl < slog.LevelError:
		return "WARN "
	default:
		return "ERROR "
	}
}
