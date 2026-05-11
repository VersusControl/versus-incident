package config

// -----------------------------------------------------------------------------
// Agent mode (AI incident detection)
// -----------------------------------------------------------------------------

type AgentConfig struct {
	Enable         bool   `mapstructure:"enable"`
	Mode           string `mapstructure:"mode"`          // training | shadow | detect
	PollInterval   string `mapstructure:"poll_interval"` // e.g. "30s"
	Lookback       string `mapstructure:"lookback"`      // e.g. "5m"
	BatchMax       int    `mapstructure:"batch_max"`
	SignalMaxBytes int    `mapstructure:"signal_max_bytes"`

	// NewServiceGrace is the duration a newly discovered service stays in
	// implicit training mode before detect-mode AI analysis begins. Signals
	// from a service still in its grace period flow through the full pipeline
	// (redact → regex → miner → catalog) but are not forwarded to the AI
	// analyzer. Set to "0" to disable (all services analysed immediately).
	NewServiceGrace string `mapstructure:"new_service_grace"` // e.g. "30m"
	// ServicePatterns is an ordered list of regexes applied to each
	// (post-redaction) log message to discover the service name.
	ServicePatterns []string `mapstructure:"service_patterns"`

	Redaction AgentRedactionConfig `mapstructure:"redaction"`
	Catalog   AgentCatalogConfig   `mapstructure:"catalog"`
	Miner     AgentMinerConfig     `mapstructure:"miner"`
	Regex     AgentRegexConfig     `mapstructure:"regex"`
	AI        AgentAIConfig        `mapstructure:"ai"`

	// SourcesPath optionally points to an external YAML file containing the
	// `sources:` list. When set, sources defined inline in the main config
	// are ignored and replaced with the file's contents at load time. The
	// external file's top-level key is `sources`. Relative paths are
	// resolved against the main config file's directory.
	SourcesPath string              `mapstructure:"sources_path"`
	Sources     []AgentSourceConfig `mapstructure:"sources"`
}

type AgentRedactionConfig struct {
	Enable        bool     `mapstructure:"enable"`
	RedactIPs     bool     `mapstructure:"redact_ips"`
	ExtraPatterns []string `mapstructure:"extra_patterns"`
}

type AgentCatalogConfig struct {
	PersistInterval  string `mapstructure:"persist_interval"`   // e.g. "30s"
	AutoPromoteAfter int    `mapstructure:"auto_promote_after"` // 0 = never
	// SpikeMultiplier flags a tick as a frequency spike when the tick
	// count exceeds the pattern's prior EWMA baseline by this factor.
	// 0 disables spike detection. Default 5.0.
	SpikeMultiplier float64 `mapstructure:"spike_multiplier"`
	// SpikeMinFrequency is the minimum tick count required before a spike
	// can fire. Avoids triggering on tiny absolute counts (e.g. baseline
	// 0.5 → tickFreq 3 is technically 6× but not interesting). Default 5.
	SpikeMinFrequency int `mapstructure:"spike_min_frequency"`
	// SpikeMinBaselineCount is the minimum total observations required on
	// a pattern before spike detection considers it. Avoids treating a
	// barely-seen pattern's first big tick as a spike. Default 20.
	SpikeMinBaselineCount int `mapstructure:"spike_min_baseline_count"`
}

// CatalogBlobName is the storage blob key used by the agent catalog.
// Backends translate this into a path / redis key / row.
const CatalogBlobName = "patterns"

// ShadowBlobName is the storage blob key used by the shadow log.
const ShadowBlobName = "shadow"

// AICacheBlobName is the storage blob key used by the AI SRE result
// cache (per-pattern findings, ttl-bounded).
const AICacheBlobName = "ai_cache"

type AgentMinerConfig struct {
	SimilarityThreshold float64 `mapstructure:"similarity_threshold"`
	TreeDepth           int     `mapstructure:"tree_depth"`
	MaxChildren         int     `mapstructure:"max_children"`
}

type AgentRegexRule struct {
	Name    string `mapstructure:"name"`
	Pattern string `mapstructure:"pattern"`
}

type AgentRegexConfig struct {
	DefaultPattern string           `mapstructure:"default_pattern"`
	Rules          []AgentRegexRule `mapstructure:"rules"`
}

type AgentSourceConfig struct {
	Name          string                         `mapstructure:"name"`
	Type          string                         `mapstructure:"type"` // "elasticsearch" | "file"
	Enable        bool                           `mapstructure:"enable"`
	Elasticsearch AgentElasticsearchSourceConfig `mapstructure:"elasticsearch"`
	File          AgentFileSourceConfig          `mapstructure:"file"`
}

// AgentFileSourceConfig drives the file-tailing SignalSource.
//
// The file source is the cheapest way to exercise the agent end-to-end: drop a
// log file on disk, point the agent at it, restart, and watch the catalog
// fill up. It tracks its position in a sidecar cursor file so it survives
// restarts and handles log rotation (file shrinks → start over from offset 0).
type AgentFileSourceConfig struct {
	// Path to the log file. Globs are NOT supported (one source = one file).
	Path string `mapstructure:"path"`
	// Format: "text" (default) or "json" (one JSON object per line).
	Format string `mapstructure:"format"`
	// CursorPath overrides the default sidecar cursor file location
	// (default: <agent.data_dir>/cursors/file-<source_name>.cursor).
	CursorPath string `mapstructure:"cursor_path"`
	// FromBeginning controls behavior when there is no cursor yet.
	// false (default) starts at the current end of the file (tail-like).
	// true reads the whole file from the start (replay-like — useful for tests).
	FromBeginning bool `mapstructure:"from_beginning"`
	// MaxLineBytes caps a single line's length to protect memory; longer
	// lines are truncated. Default 64 KiB.
	MaxLineBytes int `mapstructure:"max_line_bytes"`
	// MaxLinesPerPull caps the number of lines returned by a single Pull.
	// When the file has a large backlog (e.g. from_beginning=true on a 100k-
	// line file), this paginates the backfill across ticks so the worker's
	// batch_max truncation never silently drops unread tail content. The
	// byte offset only advances over what was actually consumed in this
	// Pull, so the next tick resumes exactly where this one stopped.
	// Default 1000.
	MaxLinesPerPull int `mapstructure:"max_lines_per_pull"`

	// Text-mode options ------------------------------------------------------

	// TimestampLayout is a Go time layout (e.g. "2006-01-02T15:04:05Z07:00")
	// used to parse a leading timestamp from each line. When empty the
	// source uses time.Now() at read time. Default tries RFC3339Nano then
	// RFC3339, then "2006-01-02 15:04:05".
	TimestampLayout string `mapstructure:"timestamp_layout"`

	// JSON-mode options ------------------------------------------------------

	MessageField   string `mapstructure:"message_field"`   // default: "message"
	TimestampField string `mapstructure:"timestamp_field"` // default: "@timestamp"
	SeverityField  string `mapstructure:"severity_field"`  // default: "level"
}

type AgentElasticsearchSourceConfig struct {
	Addresses          []string `mapstructure:"addresses"`
	Username           string   `mapstructure:"username"`
	Password           string   `mapstructure:"password"`
	APIKey             string   `mapstructure:"api_key"`
	InsecureSkipVerify bool     `mapstructure:"insecure_skip_verify"`
	Index              string   `mapstructure:"index"`
	TimeField          string   `mapstructure:"time_field"`
	Query              string   `mapstructure:"query"` // Lucene-style query string
	MessageField       string   `mapstructure:"message_field"`
	SeverityField      string   `mapstructure:"severity_field"`
	ExtraFields        []string `mapstructure:"extra_fields"`
	PageSize           int      `mapstructure:"page_size"`
}

// AgentAIConfig holds configuration for the AI SRE used in detect mode.
// The struct and env overrides are wired today; the concrete HTTP client
// and prompt builder land alongside detect-mode emission.
type AgentAIConfig struct {
	// Enable gates whether the AI SRE is called at all. When false
	// (the default) detect mode still classifies patterns but never calls
	// the LLM — it only logs what it would have sent. This allows operators
	// to run detect mode in a "dry-run" fashion without an API key.
	Enable bool `mapstructure:"enable"`
	// APIKey is the bearer token sent in the Authorization header.
	APIKey string `mapstructure:"api_key"`
	// Model is the model identifier, e.g. "gpt-4o-mini".
	Model string `mapstructure:"model"`
	// Temperature controls randomness (0.0–2.0). Default 0.2.
	Temperature float64 `mapstructure:"temperature"`
	// MaxTokens caps the response length. Default 512.
	MaxTokens int `mapstructure:"max_tokens"`
	// MaxCallsPerHour is a sliding-window rate limit. 0 = unlimited.
	MaxCallsPerHour int `mapstructure:"max_calls_per_hour"`
	// CacheTTL is how long an AI result for a pattern_id is cached to
	// avoid re-asking about the same pattern. Default "1h".
	CacheTTL string `mapstructure:"cache_ttl"`
}
