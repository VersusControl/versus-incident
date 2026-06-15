package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// modelstate.go — the learned-state / model-artifact persistence seam
// (E14). It is the OSS *place to persist* opaque, derived model state
// (learned baselines, SLO definitions, forecast models, …) keyed by org +
// agent + logical key. It is deliberately unopinionated: the bytes are
// opaque and the OSS tree never interprets them. The training loop and the
// meaning of the bytes belong to the enterprise consumer (X15/X16/X20/
// X21/X22), which keeps the intelligence private to versus-enterprise.
//
// Storage rides the existing blob seam (ReadBlob/WriteBlob — never
// os.WriteFile) under a namespaced name, and purge rides the X1-T7
// storage.Lifecycle seam. Per-org isolation is structural: the OrgID is a
// path component of the blob name, so org A's artifacts never collide with
// or resolve for org B. The enterprise consumer supplies the OrgID from
// the request context resolved by the X3 tenancy seam — no OSS change.

// ModelStateNamespace is the blob-name prefix under which every learned
// model artifact is persisted: <namespace>/<org>/<agent>/<key>.
const ModelStateNamespace = "models"

// ErrInvalidModelKey is returned when an org/agent/key component is empty
// or contains a path separator or "..", which could escape the model-state
// namespace on the file backend.
var ErrInvalidModelKey = errors.New("storage: invalid model-state key component")

// ModelState is the envelope around one persisted model artifact. Data is
// opaque to OSS; the metadata (OrgID, Agent, Key, Version, UpdatedAt) lets
// the consumer version its state and lets retention tooling reason about
// age without interpreting the payload.
type ModelState struct {
	OrgID     string    `json:"org_id"`
	Agent     string    `json:"agent"`
	Key       string    `json:"key"`
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	// Data is the opaque, consumer-defined payload, stored verbatim.
	Data []byte `json:"data"`
}

// ModelStore is a thin, unopinionated helper over a Provider that persists
// ModelState artifacts under the models/ namespace. It owns the naming
// convention and metadata stamping only; it never inspects Data.
type ModelStore struct {
	p Provider
}

// NewModelStore returns a ModelStore backed by p.
func NewModelStore(p Provider) *ModelStore {
	return &ModelStore{p: p}
}

// modelBlobName computes the namespaced blob name for one artifact:
// models/<org>/<agent>/<key>. A blank org defaults to DefaultOrgID so
// single-tenant OSS callers never have to think about orgs.
func modelBlobName(orgID, agent, key string) (string, error) {
	org := NormalizeOrgID(orgID)
	for _, c := range []string{org, agent, key} {
		if c == "" || strings.ContainsAny(c, "/\\") || strings.Contains(c, "..") {
			return "", ErrInvalidModelKey
		}
	}
	return ModelStateNamespace + "/" + org + "/" + agent + "/" + key, nil
}

// Put persists data as the artifact (org, agent, key) at version, stamping
// UpdatedAt. data is stored verbatim and opaquely; pass nil to persist an
// empty artifact. A copy of data is taken so the caller may reuse the
// slice (suspenders against a pooled request buffer being aliased into
// persisted state, golden rule #11).
func (s *ModelStore) Put(orgID, agent, key string, version int, data []byte) error {
	name, err := modelBlobName(orgID, agent, key)
	if err != nil {
		return err
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	ms := ModelState{
		OrgID:     NormalizeOrgID(orgID),
		Agent:     agent,
		Key:       key,
		Version:   version,
		UpdatedAt: time.Now().UTC(),
		Data:      cp,
	}
	enc, err := json.Marshal(&ms)
	if err != nil {
		return fmt.Errorf("storage: marshal model state %q: %w", name, err)
	}
	return s.p.WriteBlob(name, enc)
}

// Get returns the artifact (org, agent, key), or (nil, nil) when it has
// never been persisted — mirroring the ReadBlob "fresh start" contract so
// a consumer's first-run path stays a single nil check.
func (s *ModelStore) Get(orgID, agent, key string) (*ModelState, error) {
	name, err := modelBlobName(orgID, agent, key)
	if err != nil {
		return nil, err
	}
	raw, err := s.p.ReadBlob(name)
	if err != nil {
		return nil, fmt.Errorf("storage: read model state %q: %w", name, err)
	}
	if raw == nil {
		return nil, nil
	}
	var ms ModelState
	if err := json.Unmarshal(raw, &ms); err != nil {
		return nil, fmt.Errorf("storage: unmarshal model state %q: %w", name, err)
	}
	return &ms, nil
}

// Purge removes the artifact (org, agent, key) via the X1-T7
// storage.Lifecycle delete primitive. It returns ErrUnsupported when the
// backend does not implement Lifecycle (file / community), so a retention
// caller fails closed rather than silently no-op'ing. ErrNotFound is
// returned when the artifact does not exist.
func (s *ModelStore) Purge(orgID, agent, key string) error {
	name, err := modelBlobName(orgID, agent, key)
	if err != nil {
		return err
	}
	lc, ok := s.p.(Lifecycle)
	if !ok {
		return ErrUnsupported
	}
	return lc.DeleteByID(DomainBlobs, name)
}
