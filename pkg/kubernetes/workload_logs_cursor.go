package kubernetes

import "time"

// sinceTimeOverlap is how far before the caller's own timestamp a resumed source
// starts. Re-reading a little is free — a viewer drops exact duplicates by line
// identity — while missing a line at the reconnect boundary is not.
const sinceTimeOverlap = 2 * time.Second

// workloadResumeTime turns a viewer's resume timestamp into the start every
// source of a workload uses on a fresh connection. Zero means the caller did not
// resume by time, so each source starts at its own window bounds instead.
//
// The start sits a little before the watermark, so a viewer resuming at the very
// edge of the read window derives a start just outside it. That edge is
// accepted: the read clamps this derived start to the window floor and serves
// the whole window. The floor itself does not move, and a timestamp the viewer
// sends directly is still refused when it falls outside the window.
func workloadResumeTime(options WorkloadLogStreamOptions) time.Time {
	if options.SinceTime.IsZero() {
		return time.Time{}
	}
	return options.SinceTime.Add(-sinceTimeOverlap)
}

// workloadSourcePosition decides where one source begins reading. A source this
// connection already read continues from its own last line; a fresh one starts at
// the viewer's resume timestamp. Neither is ever handed to the viewer: the
// position lives for one connection and a reconnect is expressed as a timestamp.
func workloadSourcePosition(position string, resumeTime time.Time) (string, time.Time) {
	if position != "" {
		return position, time.Time{}
	}
	return "", resumeTime
}
