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
	"errors"
	"time"
)

// ErrNotFound is returned by Get* methods when the key/id is missing.
// File and memory backends translate os.ErrNotExist into ErrNotFound so
// callers can rely on errors.Is(err, storage.ErrNotFound).
var ErrNotFound = errors.New("storage: not found")

// ErrUnsupported is returned by the redis/database stub backends.
var ErrUnsupported = errors.New("storage: backend not implemented")

// Provider is the storage interface used by the agent and incident
// service.
type Provider interface {
	// ReadBlob returns the contents previously written under name.
	// Missing blobs MUST return (nil, nil) — not ErrNotFound — so the
	// agent's "fresh start" path stays a single line.
	ReadBlob(name string) ([]byte, error)
	// WriteBlob atomically replaces the blob stored under name.
	WriteBlob(name string, data []byte) error

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

	// Close releases any underlying resources (file handles, redis
	// connections, db pools). Calling Close on a closed provider is a
	// no-op.
	Close() error
}

// IncidentRecord is the durable shape of an incident. It mirrors the
// runtime models.Incident plus the audit fields the UI needs (when it
// happened, who got notified, was it acked, raw payload for debugging).
type IncidentRecord struct {
	ID       string `json:"id"`
	TeamID   string `json:"team_id,omitempty"`
	Title    string `json:"title,omitempty"`
	Source   string `json:"source,omitempty"`  // "http" | "sns" | "sqs" | ...
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
