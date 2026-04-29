package config

import "path/filepath"

// -----------------------------------------------------------------------------
// Agent mode (AI incident detection) — see local/plans/ai-incident-detection
// -----------------------------------------------------------------------------

type AgentConfig struct {
	Enable         bool   `mapstructure:"enable"`
	Mode           string `mapstructure:"mode"`          // training | shadow | detect
	PollInterval   string `mapstructure:"poll_interval"` // e.g. "30s"
	Lookback       string `mapstructure:"lookback"`      // e.g. "5m"
	BatchMax       int    `mapstructure:"batch_max"`
	SignalMaxBytes int    `mapstructure:"signal_max_bytes"`
	GatewaySecret  string `mapstructure:"gateway_secret"`
	DataDir        string `mapstructure:"data_dir"`

	Redaction AgentRedactionConfig `mapstructure:"redaction"`
	Catalog   AgentCatalogConfig   `mapstructure:"catalog"`
	Miner     AgentMinerConfig     `mapstructure:"miner"`
	Regex     AgentRegexConfig     `mapstructure:"regex"`

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
	// Mode selects the catalog storage backend. Currently only "file" is
	// supported — the catalog is persisted as `<agent.data_dir>/patterns.json`.
	// Reserved for future backends: "redis", "database", etc. The catalog
	// filename is fixed and not user-configurable.
	Mode             string `mapstructure:"mode"`
	PersistInterval  string `mapstructure:"persist_interval"`   // e.g. "30s"
	AutoPromoteAfter int    `mapstructure:"auto_promote_after"` // 0 = never
}

// CatalogFileName is the fixed filename used by the "file" catalog backend.
// Not user-configurable.
const CatalogFileName = "patterns.json"

// ResolvedDataDir returns the configured data dir or the default ("data").
func (a *AgentConfig) ResolvedDataDir() string {
	if a.DataDir == "" {
		return "data"
	}
	return a.DataDir
}

// CatalogPath returns the on-disk path used by the "file" catalog backend:
// `<data_dir>/patterns.json`. The filename is fixed.
func (a *AgentConfig) CatalogPath() string {
	return filepath.Join(a.ResolvedDataDir(), CatalogFileName)
}

type AgentMinerConfig struct {
	SimilarityThreshold float64 `mapstructure:"similarity_threshold"`
	TreeDepth           int     `mapstructure:"tree_depth"`
	MaxChildren         int     `mapstructure:"max_children"`
}

type AgentRegexRule struct {
	Name     string `mapstructure:"name"`
	Pattern  string `mapstructure:"pattern"`
	Severity string `mapstructure:"severity"`
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
