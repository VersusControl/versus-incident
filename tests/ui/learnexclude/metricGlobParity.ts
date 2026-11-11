// Disable-Learn metric glob cases: the client matcher (matchesMetricPattern)
// must agree with the Go server matcher, which is the authority. The Go
// parity test carries the same cases; keep the two tables in step.

export interface MetricGlobCase {
  name: string;
  /** The exclusion pattern. */
  entry: string;
  /** The metric signal it is matched against. */
  input: string;
  expected: boolean;
}

export const METRIC_GLOB_CASES: readonly MetricGlobCase[] = [
  { name: "exact name", entry: "up", input: "up", expected: true },
  { name: "exact is not a prefix", entry: "up", input: "upstream", expected: false },
  { name: "different exact name", entry: "up", input: "http_requests_total", expected: false },
  { name: "runtime prefix", entry: "go_*", input: "go_goroutines", expected: true },
  { name: "server prefix", entry: "prometheus_*", input: "prometheus_tsdb_samples", expected: true },
  { name: "different prefix", entry: "go_*", input: "http_requests_total", expected: false },
  { name: "prefix without wildcard", entry: "go_", input: "go_goroutines", expected: false },
  { name: "star matches empty suffix", entry: "go_*", input: "go_", expected: true },
  { name: "suffix wildcard", entry: "*_total", input: "http_requests_total", expected: true },
  { name: "suffix mismatch", entry: "*_total", input: "http_requests_count", expected: false },
  { name: "middle wildcard", entry: "http_*_total", input: "http_requests_total", expected: true },
  { name: "multiple wildcards", entry: "*:prometheus_*", input: "rate:prometheus_tsdb_samples", expected: true },
  { name: "single character wildcard", entry: "http_?xx", input: "http_5xx", expected: true },
  { name: "single character mismatch", entry: "http_?xx", input: "http_50x", expected: false },
  { name: "question mark requires a character", entry: "http_?xx", input: "http_xx", expected: false },
  { name: "question mark matches one non-ASCII rune", entry: "http_?xx", input: "http_éxx", expected: true },
  { name: "exact case sensitivity", entry: "up", input: "Up", expected: false },
  { name: "glob case sensitivity", entry: "go_*", input: "GO_gc", expected: false },
  { name: "entry whitespace trimmed", entry: "  up  ", input: "up", expected: true },
  { name: "glob whitespace trimmed", entry: "  go_*  ", input: "go_goroutines", expected: true },
  { name: "signal whitespace preserved", entry: "up", input: " up ", expected: false },
  { name: "empty entry", entry: "", input: "up", expected: false },
  { name: "blank entry", entry: "   ", input: "up", expected: false },
  { name: "empty exact signal", entry: "up", input: "", expected: false },
  { name: "empty wildcard signal", entry: "*", input: "", expected: false },
  { name: "catch-all nonempty signal", entry: "*", input: "up", expected: true },
  { name: "literal exact dot", entry: "go.gc", input: "go.gc", expected: true },
  { name: "exact dot is not regex", entry: "go.gc", input: "goxgc", expected: false },
  { name: "literal glob dot", entry: "go.gc*", input: "go.gc_duration", expected: true },
  { name: "glob dot is not regex", entry: "go.gc*", input: "goxgc_duration", expected: false },
  { name: "literal regex operators", entry: "metric[0](a)+{2}^$|\\*", input: "metric[0](a)+{2}^$|\\value", expected: true },
  { name: "brackets are not a character class", entry: "metric[0]*", input: "metric0_value", expected: false },
  { name: "start anchored", entry: "go_*", input: "prefix_go_goroutines", expected: false },
  { name: "end anchored", entry: "http_?xx", input: "http_5xx_extra", expected: false },
  { name: "star does not cross newline", entry: "go_*", input: "go_a\nb", expected: false },
  { name: "question mark does not match newline", entry: "go_?", input: "go_\n", expected: false },
];
