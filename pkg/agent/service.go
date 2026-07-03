package agent

import (
	"fmt"
	"regexp"
	"strings"
)

// ansiEscapeRe matches ANSI CSI escape sequences (colour/SGR codes and the
// like). Console loggers — Spring Boot / Logback in particular — wrap fields
// such as the application name in colour escapes (e.g.
// "\x1b[34mlead-service\x1b[m"). Those raw escape bytes sit right against the
// token, so they defeat anchors and word boundaries in the service patterns
// (the byte before "lead-service" is the SGR's trailing 'm', a word char, so
// there is no \b there). We strip them once, up front, before matching.
var ansiEscapeRe = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// logLevelRe matches a bare log-level word. A greedy positional pattern (e.g.
// "the first token after the timestamp") can capture the LEVEL instead of the
// service on some layouts, so Extract refuses to return one of these as a
// service name and falls through to the next pattern.
var logLevelRe = regexp.MustCompile(`^(?i:TRACE|DEBUG|INFO|WARN|WARNING|ERROR|FATAL)$`)

// hasLetterRe matches a capture that contains at least one ASCII letter. The
// bracket/syslog patterns capture with the class [A-Za-z0-9._-]+, which also
// matches a purely-numeric token — a thread id / PID / port / address (e.g.
// "1210", "1431", "8080", "10.0.0.1", "12-34"). Such a token is never a
// service name, so Extract refuses to return a capture with NO letter and
// falls through to the next pattern (exactly like logLevelRe). A name that
// merely CONTAINS digits still has a letter ("s3", "api-v2", "auth-service-2",
// "svc7") and is kept.
var hasLetterRe = regexp.MustCompile(`[A-Za-z]`)

// threadNameRe matches a capture that is unmistakably a JVM / servlet-container
// / reactor THREAD name rather than a service. Console loggers (Spring Boot /
// Logback in particular) print the worker thread in a bracket — e.g.
// "[nio-8080-exec-8]" or "[pool-2-thread-1]" — right where a bracket rule looks
// for a service, and because those names contain letters ("nio", "exec") the
// numeric guard (hasLetterRe) does not reject them. This third guard skips such
// a capture so Extract falls through to the next pattern (the logger / real
// service), exactly like logLevelRe and hasLetterRe.
//
// It is anchored (^…$, case-insensitive) and deliberately tight so it rejects
// ONLY clear thread-name shapes and never a legitimate service that merely
// contains digits/dashes ("api2", "auth-service", "order-service-2",
// "nio-gateway" — the "nio-<port>-exec-<n>" shape is specific enough not to
// catch that). Each alternative below targets one common thread family.
var threadNameRe = regexp.MustCompile(`(?i)^(?:` +
	// Tomcat / servlet-container connector exec threads:
	//   nio-8080-exec-8, http-nio-8080-exec-3, https-jsse-nio-8443-exec-1
	`(?:https?-)?(?:jsse-)?nio-\d+-exec-\d+` + `|` +
	//   ajp-nio-8009-exec-2 (AJP connector)
	`ajp-nio-\d+-exec-\d+` + `|` +
	//   bare exec worker: exec-4
	`exec-\d+` + `|` +
	// Standard java.util.concurrent / JVM threads:
	//   pool-2-thread-1, Thread-5
	`pool-\d+-thread-\d+` + `|` +
	`thread-\d+` + `|` +
	//   ForkJoinPool-1-worker-3, ForkJoinPool.commonPool-worker-3
	`forkjoinpool[-.].*worker[-.]?\d*` + `|` +
	// Spring task / scheduler executors:
	//   taskExecutor-1, task-3, scheduling-1
	`taskexecutor-\d+` + `|` +
	`task-\d+` + `|` +
	`scheduling-\d+` + `|` +
	// Netty / Project Reactor event loops & schedulers:
	//   nioEventLoopGroup-2-1, reactor-http-nio-4, boundedElastic-1, parallel-2
	`.*eventloopgroup-\d+-\d+` + `|` +
	`reactor-http-nio-\d+` + `|` +
	`boundedelastic-\d+` + `|` +
	`parallel-\d+` +
	`)$`)

// stripANSI removes ANSI escape sequences from s. It fast-paths the common
// case (no ESC byte at all) so it costs nothing on the vast majority of log
// lines and only pays the regex on genuinely colourised console output.
func stripANSI(s string) string {
	if !strings.ContainsRune(s, '\x1b') {
		return s
	}
	return ansiEscapeRe.ReplaceAllString(s, "")
}

// ServiceMatcher extracts a service name from a log message using an ordered
// list of regexes. The first pattern that matches wins; the FIRST capture
// group is returned. A nil/empty matcher returns "" — service detection is
// off and every signal is attributed to "_unknown" in the worker. There is
// no built-in default list: operators who want service detection MUST
// configure `agent.service_patterns` (or set `AGENT_SERVICE_PATTERNS`).
type ServiceMatcher struct {
	patterns []*regexp.Regexp
}

// NewServiceMatcher compiles the supplied regexes. Bad patterns are reported
// in the returned error slice but do not prevent the matcher from being
// built with whatever did compile, so a single typo cannot disable the
// entire pipeline. An empty/nil `patterns` list yields a matcher whose
// Extract always returns "" (service detection disabled).
func NewServiceMatcher(patterns []string) (*ServiceMatcher, []error) {
	var errs []error
	m := &ServiceMatcher{}
	for i, p := range patterns {
		if p == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("service_patterns[%d] %q: %w", i, p, err))
			continue
		}
		if re.NumSubexp() < 1 {
			errs = append(errs, fmt.Errorf("service_patterns[%d] %q: missing capture group", i, p))
			continue
		}
		m.patterns = append(m.patterns, re)
	}
	return m, errs
}

// Extract returns the first capture group of the first matching pattern, or
// "" when nothing matches.
//
// Four generic correctness guards run here so they benefit every configured
// pattern at once:
//   - ANSI escape sequences are stripped from the message before matching, so
//     a colour-wrapped token (e.g. Spring Boot's "\x1b[34mlead-service\x1b[m")
//     is matchable by ordinary patterns.
//   - A bare log-LEVEL word (TRACE/DEBUG/INFO/WARN/WARNING/ERROR/FATAL) is
//     never returned as a service. If a pattern's first group captures exactly
//     a level token, we skip it and continue to the next pattern, so a greedy
//     positional pattern cannot misattribute a signal to "DEBUG".
//   - A purely-numeric/separator token (no ASCII letter — e.g. "1210", "8080",
//     "10.0.0.1") is never returned as a service. The bracket/syslog patterns
//     capture with [A-Za-z0-9._-]+, so a bracketed thread id / PID / port like
//     "[1210]" would otherwise surface as the service; the number guard skips
//     it and continues to the next pattern. A name that merely CONTAINS digits
//     (has a letter) — "s3", "api-v2" — is kept.
//   - A JVM / servlet-container / reactor THREAD name (e.g. "nio-8080-exec-8",
//     "pool-2-thread-1", "reactor-http-nio-4") is never returned as a service.
//     Those names contain letters, so the number guard does not catch them; a
//     bracket rule would otherwise file a signal under the Tomcat worker thread
//     instead of the service. The thread-name guard skips such a capture and
//     continues to the next pattern (the logger / real service).
func (m *ServiceMatcher) Extract(message string) string {
	if m == nil || message == "" {
		return ""
	}
	message = stripANSI(message)
	for _, re := range m.patterns {
		sub := re.FindStringSubmatch(message)
		if len(sub) >= 2 && sub[1] != "" {
			if logLevelRe.MatchString(sub[1]) {
				continue
			}
			if !hasLetterRe.MatchString(sub[1]) {
				continue
			}
			if threadNameRe.MatchString(sub[1]) {
				continue
			}
			return sub[1]
		}
	}
	return ""
}
