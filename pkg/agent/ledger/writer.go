package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/egress"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/google/uuid"
)

const maxEntriesPerRun = 10000

var ErrLedgerUnavailable = errors.New("agent ledger unavailable")

type TriggerKind string

const (
	TriggerUser      TriggerKind = "user"
	TriggerIncident  TriggerKind = "incident"
	TriggerSchedule  TriggerKind = "schedule"
	TriggerCI        TriggerKind = "ci"
	TriggerMCP       TriggerKind = "mcp"
	TriggerSubagent  TriggerKind = "subagent"
)

type Subject struct {
	Kind string `json:"kind,omitempty"`
	ID   string `json:"id,omitempty"`
}

type Trigger struct {
	TriggerID      string      `json:"trigger_id"`
	RunID          string      `json:"run_id"`
	ParentRunID    string      `json:"parent_run_id,omitempty"`
	Kind           TriggerKind `json:"kind"`
	Principal      string      `json:"principal"`
	Org            string      `json:"org"`
	Surface        string      `json:"surface"`
	ClientVersion  string      `json:"client_version,omitempty"`
	Subject        Subject     `json:"subject,omitempty"`
	PolicyVersion  string      `json:"policy_version"`
	ModelVersion   string      `json:"model_version,omitempty"`
	DeciderVersion string      `json:"decider_version,omitempty"`
	SkillVersions  []string    `json:"skill_versions,omitempty"`
	At             time.Time   `json:"at"`
}

type EntryKind string

const (
	EntryTrigger        EntryKind = "trigger"
	EntryTurn           EntryKind = "turn"
	EntryToolCall       EntryKind = "tool_call"
	EntryEvidence       EntryKind = "evidence"
	EntryDecision       EntryKind = "decision"
	EntryApproval       EntryKind = "approval"
	EntryActionProposed EntryKind = "action_proposed"
	EntryActionExecuted EntryKind = "action_executed"
	EntryActionVerified EntryKind = "action_verified"
	EntryGuideIssued    EntryKind = "guide_issued"
	EntryRunClosed      EntryKind = "run_closed"
	EntryOutcome        EntryKind = "outcome"
)

type Entry struct {
	RunID   string          `json:"run_id"`
	Seq     int64           `json:"seq"`
	Kind    EntryKind       `json:"kind"`
	At      time.Time       `json:"at"`
	State   string          `json:"state"`
	Ref     string          `json:"ref,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Prev    string          `json:"prev,omitempty"`
}

type RunOutcome struct {
	State string `json:"state"`
	Code  string `json:"code,omitempty"`
}

type Writer interface {
	Begin(context.Context, Trigger) error
	Append(context.Context, Entry) (Entry, error)
	Close(context.Context, string, RunOutcome) error
}

type Run struct {
	Writer Writer
	RunID  string
}

type runContextKey struct{}

func WithRun(ctx context.Context, writer Writer, runID string) context.Context {
	return context.WithValue(ctx, runContextKey{}, Run{Writer: writer, RunID: runID})
}

func RunFromContext(ctx context.Context) (Run, bool) {
	run, ok := ctx.Value(runContextKey{}).(Run)
	return run, ok && run.Writer != nil && run.RunID != ""
}

type BlobWriter struct {
	provider storage.Provider
	org      string
	guard    egress.Guard
	now      func() time.Time
	mu       sync.Mutex
	next     map[string]int64
}

func NewBlobWriter(provider storage.Provider, org string) *BlobWriter {
	return &BlobWriter{provider: provider, org: storage.NormalizeOrgID(org), guard: egress.NewDefaultGuard(), now: time.Now, next: make(map[string]int64)}
}

func (writer *BlobWriter) Begin(ctx context.Context, trigger Trigger) error {
	if writer == nil || writer.provider == nil || trigger.RunID == "" || ctx == nil {
		return ErrLedgerUnavailable
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrLedgerUnavailable, err)
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	key := writer.triggerKey(trigger.RunID)
	previous, err := writer.provider.ReadBlob(key)
	if err != nil || previous != nil {
		return ErrLedgerUnavailable
	}
	if trigger.TriggerID == "" {
		trigger.TriggerID = uuid.NewString()
	}
	if trigger.Org == "" {
		trigger.Org = writer.org
	}
	if trigger.At.IsZero() {
		trigger.At = writer.now().UTC()
	}
	encoded, err := json.Marshal(trigger)
	if err != nil || writer.provider.WriteBlob(key, encoded) != nil {
		return ErrLedgerUnavailable
	}
	writer.next[trigger.RunID] = 1
	return nil
}

func (writer *BlobWriter) Append(ctx context.Context, entry Entry) (Entry, error) {
	if writer == nil || writer.provider == nil || ctx == nil || entry.RunID == "" || entry.Kind == "" {
		return Entry{}, ErrLedgerUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Entry{}, errors.Join(ErrLedgerUnavailable, err)
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	trigger, err := writer.provider.ReadBlob(writer.triggerKey(entry.RunID))
	if err != nil || trigger == nil {
		return Entry{}, ErrLedgerUnavailable
	}
	next, exists := writer.next[entry.RunID]
	if !exists {
		next, err = writer.nextSequence(entry.RunID)
		if err != nil {
			return Entry{}, err
		}
	}
	if next > maxEntriesPerRun {
		return Entry{}, ErrLedgerUnavailable
	}
	key := writer.entryKey(entry.RunID, next)
	previous, err := writer.provider.ReadBlob(key)
	if err != nil || previous != nil {
		return Entry{}, ErrLedgerUnavailable
	}
	entry.Seq = next
	if entry.At.IsZero() {
		entry.At = writer.now().UTC()
	}
	entry.Payload, err = writer.scrubPayload(ctx, entry.Payload)
	if err != nil {
		return Entry{}, ErrLedgerUnavailable
	}
	encoded, err := json.Marshal(entry)
	if err != nil || len(encoded) > 8*1024 {
		return Entry{}, ErrLedgerUnavailable
	}
	if err := writer.provider.WriteBlob(key, encoded); err != nil {
		return Entry{}, ErrLedgerUnavailable
	}
	writer.next[entry.RunID] = next + 1
	return entry, nil
}

func (writer *BlobWriter) Close(ctx context.Context, runID string, outcome RunOutcome) error {
	payload, err := json.Marshal(outcome)
	if err != nil {
		return ErrLedgerUnavailable
	}
	_, err = writer.Append(ctx, Entry{RunID: runID, Kind: EntryRunClosed, State: outcome.State, Payload: payload})
	return err
}

func (writer *BlobWriter) nextSequence(runID string) (int64, error) {
	for seq := int64(1); seq <= maxEntriesPerRun; seq++ {
		data, err := writer.provider.ReadBlob(writer.entryKey(runID, seq))
		if err != nil {
			return 0, ErrLedgerUnavailable
		}
		if data == nil {
			return seq, nil
		}
	}
	return 0, ErrLedgerUnavailable
}

func (writer *BlobWriter) scrubPayload(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	if len(payload) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	result, err := egress.Apply(ctx, writer.guard, egress.Request{Org: writer.org, Destination: "ledger", Parts: []egress.Part{{Kind: egress.PartToolResult, Text: string(encoded)}}})
	if err != nil {
		return nil, err
	}
	clean := json.RawMessage(result.Parts[0].Text)
	if !json.Valid(clean) {
		return nil, fmt.Errorf("invalid projected ledger payload")
	}
	return clean, nil
}

func (writer *BlobWriter) triggerKey(runID string) string {
	return "ledger/" + writer.orgKey() + "/runs/" + runID + "/trigger.json"
}

func (writer *BlobWriter) entryKey(runID string, seq int64) string {
	return fmt.Sprintf("ledger/%s/runs/%s/entries/%012d.json", writer.orgKey(), runID, seq)
}

func (writer *BlobWriter) orgKey() string {
	hash := sha256.Sum256([]byte(writer.org))
	return hex.EncodeToString(hash[:8])
}