// Package storage is the persistence layer used by the agent (pattern
// catalog, shadow log, service registry) and by the incident service
// (incident history). One Provider is constructed at boot from
// `storage:` in config.yaml and passed to every consumer that needs to
// read or write durable state.
//
// The interface is split into two concerns:
//
//   - Blob: opaque byte slices keyed by short name. Used by the agent
//     catalog and shadow log, both of which already serialize themselves
//     to JSON. Backends translate the name into a file path / Redis key
//     / row.
//   - Incident: first-class CRUD-ish operations because the UI lists,
//     filters, and acks incidents.
//
// Backends today:
//   - file (pkg/storage/file.go) — production
//   - memory (pkg/storage/memory.go) — tests only
//   - redis (pkg/storage/redis.go) — config stub, returns ErrUnsupported
//   - database (pkg/storage/database.go) — config stub
package storage

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// ErrNotFound is returned by Get* methods when the key/id is missing.
// File and memory backends translate os.ErrNotExist into ErrNotFound so
// callers can rely on errors.Is(err, storage.ErrNotFound).
var ErrNotFound = errors.New("storage: not found")

// ErrUnsupported is returned by the redis/database stub backends, and by
// the model-state purge path when the configured backend does not
// implement the optional storage.Lifecycle capability.
var ErrUnsupported = errors.New("storage: backend not implemented")

// ErrUnknownDomain is returned by Lifecycle methods when domain is not
// one of the fixed {DomainIncidents, DomainAnalyses, DomainBlobs} set.
var ErrUnknownDomain = errors.New("storage: unknown lifecycle domain")

// Lifecycle domains. A small fixed set so a backend can map each to its
// physical table(s) without ever trusting caller-supplied input.
const (
	// DomainIncidents targets incident history (IncidentRecord); age is
	// compared against created_at.
	DomainIncidents = "incidents"
	// DomainAnalyses targets analyze-mode runs (AnalysisRecord); age is
	// compared against requested_at.
	DomainAnalyses = "analyses"
	// DomainBlobs targets opaque blobs written via WriteBlob — including
	// the learned model-state artifacts under the models/ namespace (E14);
	// age is compared against the blob's updated_at.
	DomainBlobs = "blobs"
)

// DefaultDataDir is the application's persistent data directory. The
// `file` storage backend persists its JSON here (incidents, pattern
// catalog, shadow/detect logs, AI cache, runbook corpus); on-disk assets
// that are independent of the configured storage backend also live here,
// such as the runbook source files under runbooks/. It is relative to the
// process working directory; in the container image (WORKDIR /app) it
// resolves to /app/data.
const DefaultDataDir = "data"

// DefaultOrgID is the org every record carries when no explicit
// organization is supplied. Single-tenant OSS deployments never set an
// org, so every persisted record transparently belongs to "default" and
// no behaviour changes. Multi-tenant scoping (enterprise) overrides this
// per request via the org-injection seam (pkg/middleware).
const DefaultOrgID = "default"

// NormalizeOrgID returns a non-empty org id, falling back to
// DefaultOrgID when s is blank. Backends call this on the persistence
// path so a record is never stored with an empty OrgID.
func NormalizeOrgID(s string) string {
	if s == "" {
		return DefaultOrgID
	}
	return s
}

// Provider is the storage interface used by the agent and incident
// service.
type Provider interface {
	// ReadBlob returns the contents previously written under name.
	// Missing blobs MUST return (nil, nil) — not ErrNotFound — so the
	// agent's "fresh start" path stays a single line.
	ReadBlob(name string) ([]byte, error)
	// WriteBlob atomically replaces the blob stored under name.
	WriteBlob(name string, data []byte) error
	// ListBlobs returns every blob whose name begins with prefix, as
	// (name, data) pairs in no guaranteed order. It is the enumeration
	// primitive the model-state seam (ModelStore.List) rides to list all
	// learned artifacts under a namespace (models/<org>/<agent>/…). A
	// prefix that matches nothing returns an empty result, never
	// ErrNotFound, mirroring the ReadBlob "fresh start" contract. The
	// returned Data slices are copies the caller owns. The list is
	// unbounded — a namespace is naturally bounded per org × agent (one
	// artifact per learned key), so callers that need a cap apply it.
	ListBlobs(prefix string) ([]Blob, error)

	// SaveIncident appends a new incident to the store. Subsequent
	// SaveIncident calls with the same ID overwrite the existing record
	// (used by the ack path). Implementations are responsible for
	// trimming the history to a sane upper bound — the file backend
	// caps at MaxIncidents.
	SaveIncident(rec *IncidentRecord) error
	// UpdateIncidentAck stamps an existing incident as acknowledged.
	// Returns ErrNotFound when the id is unknown.
	UpdateIncidentAck(id string, ackedAt time.Time) error
	// GetIncident returns one incident or ErrNotFound.
	GetIncident(id string) (*IncidentRecord, error)
	// ListIncidents returns the most recent incidents, newest first.
	// limit <= 0 returns the full window.
	ListIncidents(limit int) ([]*IncidentRecord, error)

	// SaveAnalysis stores (or replaces by ID) one analyze-mode run.
	SaveAnalysis(rec *AnalysisRecord) error
	// GetAnalysis returns one analysis or ErrNotFound.
	GetAnalysis(id string) (*AnalysisRecord, error)
	// ListAnalysesByIncident returns all analyses for one incident,
	// newest first. limit <= 0 returns the full window.
	ListAnalysesByIncident(incidentID string, limit int) ([]*AnalysisRecord, error)
	// ListAnalyses returns analyses across all incidents, newest first.
	// limit <= 0 returns the full window.
	ListAnalyses(limit int) ([]*AnalysisRecord, error)
	// DeleteAnalysis removes one analysis. Returns ErrNotFound when the
	// id is unknown.
	DeleteAnalysis(id string) error

	// Close releases any underlying resources (file handles, redis
	// connections, db pools). Calling Close on a closed provider is a
	// no-op.
	Close() error
}

// Blob is one entry returned by Provider.ListBlobs: a stored blob's
// logical name (the same name passed to WriteBlob/ReadBlob) and its raw
// bytes. Data is a copy the caller owns and may mutate freely.
type Blob struct {
	Name string
	Data []byte
}

// Searcher is an optional capability a backend may implement on top of
// Provider. It exposes full-text-style search over incidents and
// analyses. Backends that cannot search efficiently (memory, file) do
// not implement it; callers type-assert and fall back to ListIncidents
// when the assertion fails. The Postgres backend implements it.
type Searcher interface {
	// SearchIncidents returns incidents whose title, service, source,
	// or JSON body match the case-insensitive query, newest first.
	// An empty query returns the most recent incidents (same as
	// ListIncidents). limit <= 0 returns the full window.
	SearchIncidents(query string, limit int) ([]*IncidentRecord, error)
	// SearchAnalyses returns analyses whose JSON body matches the
	// case-insensitive query, newest first. limit <= 0 returns the
	// full window.
	SearchAnalyses(query string, limit int) ([]*AnalysisRecord, error)
}

// Lifecycle is an optional capability a backend may implement on top of
// Provider (X1-T7). It is a mechanical, tier-neutral delete primitive: it
// carries NO org or policy concept — the caller decides what to purge and
// when. The enterprise retention policy engine consumes it; single-tenant
// OSS may call it directly. Backends that cannot delete efficiently (file,
// redis stub) do not implement it; callers type-assert and treat a failed
// assertion as "purge unsupported" rather than silently succeeding. The
// Postgres and memory backends implement it.
type Lifecycle interface {
	// PurgeOlderThan deletes every record in domain whose natural age
	// column is strictly older than cutoff, returning the number deleted.
	// The compared column is fixed per domain (see DomainIncidents /
	// DomainAnalyses / DomainBlobs). An unknown domain returns
	// ErrUnknownDomain and deletes nothing.
	PurgeOlderThan(domain string, cutoff time.Time) (int, error)
	// DeleteByID deletes one record from domain by its primary key — an
	// incident/analysis id, or a blob name. Returns ErrNotFound when the
	// id is unknown and ErrUnknownDomain for a domain outside the fixed
	// set.
	DeleteByID(domain, id string) error
}

// BlobCreator is an optional capability a backend may implement on top of
// Provider (X9-T11). It adds a single atomic create-if-absent blob write
// used to elect ONE writer across multiple instances that share a store —
// the substrate for generate-once secrets under HA / multi-instance, where
// every replica boots the same generate-then-persist path and exactly one
// must win. It is mechanical and tier-neutral: it carries no org or policy
// concept and never overwrites an existing blob, so it is strictly additive
// and does not weaken WriteBlob's last-write-wins semantics.
//
// Backends that cannot create atomically (the redis/database stubs) do not
// implement it; callers type-assert and treat a failed assertion as
// "unsupported", exactly like Lifecycle/Searcher. The Postgres (shared,
// multi-writer) and memory (tests) backends implement it because they are
// the HA substrate and the test path; the file backend implements it
// best-effort via O_CREATE|O_EXCL, which is coherent only on a single node —
// the only place file storage is allowed under HA (see the X9-T3
// file-storage guard).
type BlobCreator interface {
	// CreateBlobIfAbsent atomically writes data under key only if key does
	// not already exist. It returns written==true when THIS call created the
	// blob, and written==false when the key already existed — because a
	// concurrent or prior writer won the race, OR because the key was set by
	// an earlier WriteBlob. On written==false the stored bytes are left
	// untouched: the caller re-reads them via ReadBlob(key) to adopt the
	// surviving value. After a nil-error return the blob is durably present,
	// so ReadBlob(key) observes the one surviving set of bytes regardless of
	// which caller won.
	CreateBlobIfAbsent(key string, data []byte) (written bool, err error)
}

// IncidentRecord is the durable shape of an incident. It mirrors the
// runtime models.Incident plus the audit fields the UI needs (when it
// happened, who got notified, was it acked, raw payload for debugging).
type IncidentRecord struct {
	ID string `json:"id"`
	// OrgID scopes the record to one organization. Defaults to
	// storage.DefaultOrgID ("default") so single-tenant OSS users never
	// see or set it; enterprise multi-tenant routing reads it to isolate
	// orgs.
	OrgID    string `json:"org_id,omitempty"`
	TeamID   string `json:"team_id,omitempty"`
	Title    string `json:"title,omitempty"`
	Source   string `json:"source,omitempty"`  // "webhook" | "sns" | "sqs" | ...
	Service  string `json:"service,omitempty"` // best-effort from payload
	Resolved bool   `json:"resolved"`
	// ChannelsEnabled is the snapshot of channels that were configured
	// when the alert fired. ChannelsNotified is the subset that
	// actually succeeded. The two diverge whenever a channel fails:
	// keep both so the UI can show "Slack failed" without losing the
	// fact that Slack was supposed to be tried.
	ChannelsEnabled  []string `json:"channels_enabled,omitempty"`
	ChannelsNotified []string `json:"channels_notified,omitempty"`
	OnCallTriggered  bool     `json:"oncall_triggered,omitempty"`
	OnCallError      string   `json:"oncall_error,omitempty"`
	// NotifyStatus reflects the outcome of the alert fan-out:
	// "pending" — record persisted, fan-out not yet attempted
	// "sent"    — every enabled channel returned success
	// "partial" — at least one channel succeeded, at least one failed
	// "failed"  — no channel succeeded
	NotifyStatus string                 `json:"notify_status,omitempty"`
	NotifyError  string                 `json:"notify_error,omitempty"`
	CreatedAt    time.Time              `json:"created_at"`
	AckedAt      *time.Time             `json:"acked_at,omitempty"`
	ResolvedAt   *time.Time             `json:"resolved_at,omitempty"`
	Content      map[string]interface{} `json:"content,omitempty"`

	// AssignedTeamID and AssignedMemberIDs record an operator's
	// assignment for this incident. Routing logic (Phase 2) will read
	// these to pick channels per assignee; the storage layer only
	// holds the references. Empty means unassigned.
	AssignedTeamID    string   `json:"assigned_team_id,omitempty"`
	AssignedMemberIDs []string `json:"assigned_member_ids,omitempty"`
}

// AnalysisRecord is the durable shape of one analyze-mode run. The
// admin /analyze endpoint creates one per request; the UI lists them
// per incident.
type AnalysisRecord struct {
	ID string `json:"id"`
	// OrgID scopes the analysis to one organization. Defaults to
	// storage.DefaultOrgID ("default"); see IncidentRecord.OrgID.
	OrgID       string    `json:"org_id,omitempty"`
	IncidentID  string    `json:"incident_id"`
	RequestedAt time.Time `json:"requested_at"`
	RequestedBy string    `json:"requested_by,omitempty"`
	DurationMs  int64     `json:"duration_ms,omitempty"`
	Model       string    `json:"model,omitempty"`

	// ToolCalls is the full audit trail of read-only tool invocations
	// the agent issued during the run, in execution order.
	ToolCalls []AnalysisToolCall `json:"tool_calls,omitempty"`

	// Finding is the parsed structured output from the model. Nil when
	// the run failed before the model produced JSON.
	Finding *core.AIFinding `json:"finding,omitempty"`

	// RawResponse is the model's final assistant message. Kept for
	// audit / debugging when ParseFinding fails.
	RawResponse string `json:"raw_response,omitempty"`

	// Status is one of: "ok", "error", "rate_limited".
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// AnalysisToolCall captures one tool round-trip for the audit log.
type AnalysisToolCall struct {
	Name       string          `json:"name"`
	Args       json.RawMessage `json:"args,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	DurationMs int64           `json:"duration_ms,omitempty"`
	Error      string          `json:"error,omitempty"`
}
