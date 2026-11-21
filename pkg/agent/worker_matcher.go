package agent

import (
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
)

// shadowTag re-derives the regex rule name for an observation from its first
// representative signal. It is used only for audit/shadow attribution and for
// the discovery log line, mirroring the tag the pre-seam worker captured from
// the bucket's first signal. Returns "" when no matcher is configured or the
// observation carries no raw samples (e.g. a metric/trace observation), so the
// field is simply omitted downstream.
func (w *Worker) shadowTag(o core.Observation) string {
	if w.matcher == nil || len(o.Samples) == 0 {
		return ""
	}
	return w.matcher.Match(o.Samples[0].Message).RuleName
}

// matcherForSource resolves the regex pre-filter for a source by name,
// applying the per-source-KIND precedence (highest → lowest):
//
//  1. the OPTIONAL top-level per-kind override — agent.regex.metrics /
//     agent.regex.traces (metricsMatcher / tracesMatcher), when set;
//  2. the per-kind built-in default — match-all for metrics/traces;
//  3. the global logs matcher (w.matcher) for the logs kind and any
//     unknown/unregistered type. KindOf reports KindUnknown for an unknown
//     type, which reaches the switch default and uses the logs matcher.
//
// Only the log-brain path consults this; metric/trace sources bound to their
// proper enterprise brain group by fields and never hit it. The per-kind
// override still bites those sources via preBrainMatcher, which the
// worker applies to the signals before any brain (enterprise or log) sees them.
func (w *Worker) matcherForSource(name string) *RegexMatcher {
	switch w.kindByName[name] {
	case signalsources.KindMetrics:
		if w.metricsMatcher != nil {
			return w.metricsMatcher
		}
		return w.matchAll
	case signalsources.KindTraces:
		if w.tracesMatcher != nil {
			return w.tracesMatcher
		}
		return w.matchAll
	default:
		return w.matcher
	}
}

// preBrainMatcher returns the OPTIONAL per-kind text pre-filter applied to a
// metrics/traces source's signals BEFORE they reach its brain (enterprise or
// log). It is non-nil only when the operator set agent.regex.metrics /
// agent.regex.traces for that kind; logs and the learn-all
// default return nil, so no pre-filter runs — logs are filtered inside the log
// brain, and an unset override lets metrics/traces learn all. Applying the
// override here (rather than inside a brain) makes it bite brain-agnostically,
// which is the only way it can narrow a licensed source that binds to its
// enterprise brain and never builds a log brain.
func (w *Worker) preBrainMatcher(name string) *RegexMatcher {
	switch w.kindByName[name] {
	case signalsources.KindMetrics:
		return w.metricsMatcher
	case signalsources.KindTraces:
		return w.tracesMatcher
	default:
		return nil
	}
}
