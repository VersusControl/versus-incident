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

	// SourceBackoffMax caps the per-source circuit-breaker backoff. A source
	// that keeps failing is retried on an exponential backoff (starting at
	// the poll interval) up to this ceiling, instead of every tick. Empty
	// falls back to 15m. e.g. "15m", "1h".
	SourceBackoffMax string `mapstructure:"source_backoff_max"`

	Redaction AgentRedactionConfig `mapstructure:"redaction"`
	Catalog   AgentCatalogConfig   `mapstructure:"catalog"`
	Miner     AgentMinerConfig     `mapstructure:"miner"`
	Regex     AgentRegexConfig     `mapstructure:"regex"`
	AI        AgentAIConfig        `mapstructure:"ai"`

	// Sources is the list of enabled signal sources. Versus loads it from
	// the file `agent_sources.yaml` sitting next to the main config file
	// (path is hardcoded; missing file is OK — the agent simply has no
	// sources). The file's top-level key is `sources`. ${VAR} references
	// in the file are expanded against the process environment.
	Sources []AgentSourceConfig `mapstructure:"sources"`

	// Tools holds per-tool configuration for analyze-mode tools that need
	// external data (e.g. the `recent_changes` tool's git repository and
	// the `describe_dependencies` tool's service-dependency graph).
	// Versus loads it from the file `tools.yaml` sitting next to the main
	// config file (path is hardcoded; missing file is OK — tools needing
	// config then simply degrade to a clean "nothing found"). The file's
	// top-level key is `tools`. This is per-tool DATA config, not a
	// registration allow-list: tools are still registered in code via
	// `analyzetools.Default`.
	Tools ToolsConfig `mapstructure:"tools"`
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
	Name           string                          `mapstructure:"name"`
	Type           string                          `mapstructure:"type"` // "elasticsearch" | "file" | "loki" | "cloudwatchlogs" | "graylog" | "splunk"
	Enable         bool                            `mapstructure:"enable"`
	Elasticsearch  AgentElasticsearchSourceConfig  `mapstructure:"elasticsearch"`
	File           AgentFileSourceConfig           `mapstructure:"file"`
	Loki           AgentLokiSourceConfig           `mapstructure:"loki"`
	CloudWatchLogs AgentCloudWatchLogsSourceConfig `mapstructure:"cloudwatchlogs"`
	Graylog        AgentGraylogSourceConfig        `mapstructure:"graylog"`
	Splunk         AgentSplunkSourceConfig         `mapstructure:"splunk"`
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
	// (default: a ".versus-cursor-<source_name>" file next to the watched log file).
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

// AgentLokiSourceConfig drives the Grafana Loki SignalSource.
//
// The source uses Loki's HTTP `query_range` endpoint with `direction=forward`
// so the stream is read oldest-first and pagination is cursor-friendly. The
// cursor is the maximum log timestamp seen on the previous tick.
type AgentLokiSourceConfig struct {
	// Address is the Loki base URL, e.g. "http://loki:3100".
	Address string `mapstructure:"address"`
	// TenantID, when set, is sent as the `X-Scope-OrgID` header (multi-tenant
	// Loki / Grafana Cloud).
	TenantID string `mapstructure:"tenant_id"`
	// Username/Password enable HTTP Basic auth (Grafana Cloud uses
	// instance ID / API token).
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	// BearerToken is sent as `Authorization: Bearer <token>` when set.
	// Mutually exclusive with Username/Password (Bearer wins).
	BearerToken        string `mapstructure:"bearer_token"`
	InsecureSkipVerify bool   `mapstructure:"insecure_skip_verify"`
	// Query is a LogQL selector, e.g. `{app="api",env="prod"} |= "error"`.
	// Required.
	Query string `mapstructure:"query"`
	// SeverityField, when set, is read from each entry's stream labels
	// (e.g. "level") to populate Signal.Severity.
	SeverityField string `mapstructure:"severity_field"`
	// ExtraLabels are additional stream labels copied into Signal.Fields.
	ExtraLabels []string `mapstructure:"extra_labels"`
	// PageSize is the per-query limit (Loki caps this around 5000 by
	// default). Default 500.
	PageSize int `mapstructure:"page_size"`
}

// AgentCloudWatchLogsSourceConfig drives the AWS CloudWatch Logs SignalSource.
//
// The source uses the `FilterLogEvents` API (cheap, real-time, no async
// query lifecycle). Authentication is the standard AWS SDK chain
// (env vars, shared credentials file, IAM role).
type AgentCloudWatchLogsSourceConfig struct {
	// Region, e.g. "us-east-1". Required.
	Region string `mapstructure:"region"`
	// LogGroupName is the CloudWatch log group, e.g. "/aws/lambda/my-fn".
	// Required.
	LogGroupName string `mapstructure:"log_group_name"`
	// LogStreamPrefix optionally restricts events to streams whose name
	// starts with this prefix (cheaper than scanning the whole group).
	LogStreamPrefix string `mapstructure:"log_stream_prefix"`
	// FilterPattern is a CloudWatch Logs filter pattern (NOT a regex).
	// See AWS docs. Empty matches every event.
	FilterPattern string `mapstructure:"filter_pattern"`
	// PageSize is the per-call limit (max 10000). Default 500.
	PageSize int `mapstructure:"page_size"`
}

// AgentGraylogSourceConfig drives the Graylog SignalSource.
//
// The source uses Graylog's `search/universal/absolute` REST endpoint —
// synchronous, sorted by timestamp ascending, and cursor-friendly.
type AgentGraylogSourceConfig struct {
	// Address is the Graylog base URL, e.g. "https://graylog:9000".
	Address string `mapstructure:"address"`
	// Username + Password for HTTP Basic auth. Standard Graylog login.
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`
	// APIToken is an alternative to Username/Password. When set the
	// source sends Basic auth with the token as the username and the
	// literal string "token" as the password (Graylog convention).
	APIToken           string `mapstructure:"api_token"`
	InsecureSkipVerify bool   `mapstructure:"insecure_skip_verify"`
	// Query is a Graylog search string, e.g. `level:ERROR AND service:api`.
	// Defaults to "*" (match all) when empty.
	Query string `mapstructure:"query"`
	// StreamID optionally restricts the search to a single stream
	// (Graylog stream ids look like "000000000000000000000001").
	StreamID string `mapstructure:"stream_id"`
	// MessageField is the field copied into Signal.Message.
	// Default "message".
	MessageField string `mapstructure:"message_field"`
	// SeverityField is the field copied into Signal.Severity.
	// Default "level".
	SeverityField string `mapstructure:"severity_field"`
	// Fields, when non-empty, restricts the server-side projection to
	// these fields (faster + smaller responses).
	Fields []string `mapstructure:"fields"`
	// ExtraFields are additional fields copied into Signal.Fields. They
	// must also appear in Fields (or Fields must be empty so the server
	// returns the whole document).
	ExtraFields []string `mapstructure:"extra_fields"`
	// PageSize caps results per tick (Graylog default cap is 150).
	// Default 500.
	PageSize int `mapstructure:"page_size"`
}

// AgentSplunkSourceConfig drives the Splunk SignalSource.
//
// The source uses Splunk's `search/v2/jobs/export` REST endpoint for
// streaming JSON results. Auth uses a bearer token by default (HEC /
// auth tokens); HTTP Basic is the fallback for username/password setups.
type AgentSplunkSourceConfig struct {
	// Address is the Splunk REST base URL, e.g. "https://splunk:8089".
	Address string `mapstructure:"address"`
	// Token is sent as `Authorization: Bearer <token>` and takes
	// priority over Username/Password when set.
	Token string `mapstructure:"token"`
	// Username + Password fall back to HTTP Basic auth.
	Username           string `mapstructure:"username"`
	Password           string `mapstructure:"password"`
	InsecureSkipVerify bool   `mapstructure:"insecure_skip_verify"`
	// Search is the SPL query. May start with the `search` command
	// (added automatically when missing). Example:
	// `index=main sourcetype=api level=error`.
	Search string `mapstructure:"search"`
	// TimeField is the timestamp field on each result. Default "_time".
	TimeField string `mapstructure:"time_field"`
	// MessageField is the field copied into Signal.Message.
	// Default "_raw".
	MessageField string `mapstructure:"message_field"`
	// SeverityField is the field copied into Signal.Severity. Empty by
	// default — Splunk events do not have a canonical severity field.
	SeverityField string `mapstructure:"severity_field"`
	// ExtraFields are copied into Signal.Fields.
	ExtraFields []string `mapstructure:"extra_fields"`
	// PageSize caps results per tick. Default 500.
	PageSize int `mapstructure:"page_size"`
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

	// Detect and Analyze are per-task overrides. Empty fields fall back
	// to the top-level defaults above, so a single shared block keeps
	// working unchanged. Detect is used by the worker for unknown /
	// spiking pattern classification; Analyze is used by the on-demand
	// analyzer (E4).
	//
	// There is no `framework` knob: Eino is the only LLM path.
	Detect  AgentAITaskConfig    `mapstructure:"detect"`
	Analyze AgentAIAnalyzeConfig `mapstructure:"analyze"`
}

// AgentAIAnalyzeConfig is the analyze-agent override block. The model
// knob lets analyze deep dives point at a stronger model than the
// shared ai.model. All other LLM settings (temperature, max_tokens,
// rate limit, cache) are inherited from the top-level ai block, and the
// tool-loop knobs (tool_timeout, parallel_tools) live on the shared
// tools block (tools.yaml). The analyze agent is constructed whenever
// AI.Enable is true — there is no separate opt-in gate.
//
// The Model knob is read directly from cfg.AI.Analyze (NOT via Resolve,
// which zeroes the Analyze overlay), so it must be deep-cloned in
// clone_config.go.
type AgentAIAnalyzeConfig struct {
	// Model overrides the shared ai.model for analyze deep dives.
	// Empty inherits ai.model.
	Model string `mapstructure:"model"`
}

// AgentAITaskConfig is the per-task override block. Zero values mean
// "inherit the top-level AgentAIConfig field".
type AgentAITaskConfig struct {
	Model           string  `mapstructure:"model"`
	Temperature     float64 `mapstructure:"temperature"`
	MaxTokens       int     `mapstructure:"max_tokens"`
	MaxCallsPerHour int     `mapstructure:"max_calls_per_hour"`
	CacheTTL        string  `mapstructure:"cache_ttl"`
}

// Resolve returns a fully-defaulted AgentAIConfig for the given task by
// overlaying the task's sub-config onto the top-level defaults. Empty
// sub-config fields preserve the top-level value (and Temperature == 0
// is treated as "inherit" — operators wanting deterministic detect
// must set it on the top-level block).
func (c AgentAIConfig) Resolve(task AgentAITaskConfig) AgentAIConfig {
	out := c
	// Per-task block must NOT leak into Resolve's output — callers
	// receive a flat AgentAIConfig.
	out.Detect = AgentAITaskConfig{}
	out.Analyze = AgentAIAnalyzeConfig{}

	if task.Model != "" {
		out.Model = task.Model
	}
	if task.Temperature != 0 {
		out.Temperature = task.Temperature
	}
	if task.MaxTokens != 0 {
		out.MaxTokens = task.MaxTokens
	}
	if task.MaxCallsPerHour != 0 {
		out.MaxCallsPerHour = task.MaxCallsPerHour
	}
	if task.CacheTTL != "" {
		out.CacheTTL = task.CacheTTL
	}
	return out
}
