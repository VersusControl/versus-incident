package storage

import (
	"context"
	"time"
)

// Observe returns provider with the error of every call it makes passed
// through classify, which returns the error the caller gets. A consumer uses
// it to name failures for the store, in one place for every call.
//
// Callers choose their behaviour by asking a provider for optional
// capabilities, so the result keeps these when, and only when, provider has
// them:
//   - BlobCAS, with CompareAndSwapBlobContext routed to provider's own
//     context form when it has one;
//   - BlobDeleter;
//   - EvidenceStore, and with it EvidenceAuthorityStore;
//   - EvidenceScopeLookup, when provider is also an EvidenceStore (the lookup
//     is only ever asked of an evidence store).
//
// The result always has ReadBlobContext and WriteBlobContext, which use
// provider's own context forms when it has them and otherwise behave as
// ReadBlobContext and WriteBlobContext in this package do. Every other
// optional capability (BlobCreator, the paged blob and incident listers,
// SQLAccessor and the rest) is not kept. Observing an observed provider
// returns it unchanged.
//
// A call that takes a context whose deadline has already passed, or that was
// already cancelled, returns the context's error without reaching provider or
// classify: the caller stopped waiting before the store was asked.
func Observe(provider Provider, classify func(error) error) Provider {
	if provider == nil || classify == nil {
		return provider
	}
	if _, observed := provider.(observedHolder); observed {
		return provider
	}
	base := &observed{inner: provider, classify: classify}
	cas, hasCAS := provider.(BlobCAS)
	deleter, hasDelete := provider.(BlobDeleter)
	evidence, hasEvidence := provider.(EvidenceStore)
	lookup, hasLookup := provider.(EvidenceScopeLookup)
	c := observedCAS{base, cas}
	d := observedDelete{base, deleter}
	e := observedEvidence{base, evidence}
	l := observedLookup{base, lookup}
	switch {
	case hasEvidence && hasLookup && hasCAS && hasDelete:
		return &struct {
			*observed
			observedCAS
			observedDelete
			observedEvidence
			observedLookup
		}{base, c, d, e, l}
	case hasEvidence && hasLookup && hasCAS:
		return &struct {
			*observed
			observedCAS
			observedEvidence
			observedLookup
		}{base, c, e, l}
	case hasEvidence && hasLookup && hasDelete:
		return &struct {
			*observed
			observedDelete
			observedEvidence
			observedLookup
		}{base, d, e, l}
	case hasEvidence && hasLookup:
		return &struct {
			*observed
			observedEvidence
			observedLookup
		}{base, e, l}
	case hasEvidence && hasCAS && hasDelete:
		return &struct {
			*observed
			observedCAS
			observedDelete
			observedEvidence
		}{base, c, d, e}
	case hasEvidence && hasCAS:
		return &struct {
			*observed
			observedCAS
			observedEvidence
		}{base, c, e}
	case hasEvidence && hasDelete:
		return &struct {
			*observed
			observedDelete
			observedEvidence
		}{base, d, e}
	case hasEvidence:
		return &struct {
			*observed
			observedEvidence
		}{base, e}
	case hasCAS && hasDelete:
		return &struct {
			*observed
			observedCAS
			observedDelete
		}{base, c, d}
	case hasCAS:
		return &struct {
			*observed
			observedCAS
		}{base, c}
	case hasDelete:
		return &struct {
			*observed
			observedDelete
		}{base, d}
	default:
		return base
	}
}

// Unobserved returns the provider an observed provider stands in front of.
func Unobserved(provider Provider) Provider {
	if holder, ok := provider.(observedHolder); ok {
		return holder.observedBase().inner
	}
	return provider
}

// observedHolder is implemented by every type Observe returns. Its method name
// must not match any embedded field, or the embedding would hide it.
type observedHolder interface{ observedBase() *observed }

type observed struct {
	inner    Provider
	classify func(error) error
}

func (provider *observed) observedBase() *observed { return provider }

func (provider *observed) ReadBlob(name string) ([]byte, error) {
	data, err := provider.inner.ReadBlob(name)
	return data, provider.classify(err)
}

func (provider *observed) WriteBlob(name string, data []byte) error {
	return provider.classify(provider.inner.WriteBlob(name, data))
}

func (provider *observed) ReadBlobContext(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		// The caller stopped waiting before the call: not a store answer.
		return nil, err
	}
	data, err := ReadBlobContext(ctx, provider.inner, name)
	return data, provider.classify(err)
}

func (provider *observed) WriteBlobContext(ctx context.Context, name string, data []byte) error {
	if err := ctx.Err(); err != nil {
		// The caller stopped waiting before the call: not a store answer.
		return err
	}
	return provider.classify(WriteBlobContext(ctx, provider.inner, name, data))
}

func (provider *observed) ListBlobs(prefix string) ([]Blob, error) {
	blobs, err := provider.inner.ListBlobs(prefix)
	return blobs, provider.classify(err)
}

func (provider *observed) SaveIncident(record *IncidentRecord) error {
	return provider.classify(provider.inner.SaveIncident(record))
}

func (provider *observed) UpdateIncidentAck(id string, at time.Time) error {
	return provider.classify(provider.inner.UpdateIncidentAck(id, at))
}

func (provider *observed) GetIncident(id string) (*IncidentRecord, error) {
	record, err := provider.inner.GetIncident(id)
	return record, provider.classify(err)
}

func (provider *observed) ListIncidents(limit int) ([]*IncidentRecord, error) {
	records, err := provider.inner.ListIncidents(limit)
	return records, provider.classify(err)
}

func (provider *observed) SaveAnalysis(record *AnalysisRecord) error {
	return provider.classify(provider.inner.SaveAnalysis(record))
}

func (provider *observed) GetAnalysis(id string) (*AnalysisRecord, error) {
	record, err := provider.inner.GetAnalysis(id)
	return record, provider.classify(err)
}

func (provider *observed) ListAnalysesByIncident(incidentID string, limit int) ([]*AnalysisRecord, error) {
	records, err := provider.inner.ListAnalysesByIncident(incidentID, limit)
	return records, provider.classify(err)
}

func (provider *observed) ListAnalyses(limit int) ([]*AnalysisRecord, error) {
	records, err := provider.inner.ListAnalyses(limit)
	return records, provider.classify(err)
}

func (provider *observed) DeleteAnalysis(id string) error {
	return provider.classify(provider.inner.DeleteAnalysis(id))
}

func (provider *observed) Close() error { return provider.inner.Close() }

type observedCAS struct {
	base *observed
	cas  BlobCAS
}

func (part observedCAS) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	swapped, err := part.cas.CompareAndSwapBlob(name, expected, replacement)
	return swapped, part.base.classify(err)
}

func (part observedCAS) CompareAndSwapBlobContext(ctx context.Context, name string, expected, replacement []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		// The caller stopped waiting before the call: not a store answer.
		return false, err
	}
	swapped, err := CompareAndSwapBlobContext(ctx, part.cas, name, expected, replacement)
	return swapped, part.base.classify(err)
}

type observedDelete struct {
	base    *observed
	deleter BlobDeleter
}

func (part observedDelete) DeleteBlob(name string) error {
	return part.base.classify(part.deleter.DeleteBlob(name))
}

type observedEvidence struct {
	base     *observed
	evidence EvidenceStore
}

func (part observedEvidence) SetEvidenceAuthority(ctx context.Context, scope EvidenceScope, principal string, authority map[string]EvidenceAuthority) error {
	if err := ctx.Err(); err != nil {
		// The caller stopped waiting before the call: not a store answer.
		return err
	}
	return part.base.classify(part.evidence.SetEvidenceAuthority(ctx, scope, principal, authority))
}

func (part observedEvidence) EvidencePolicy() EvidencePolicy {
	return part.evidence.EvidencePolicy()
}

func (part observedEvidence) CommitEvidence(ctx context.Context, scope EvidenceScope, batch EvidenceBatch) error {
	if err := ctx.Err(); err != nil {
		// The caller stopped waiting before the call: not a store answer.
		return err
	}
	return part.base.classify(part.evidence.CommitEvidence(ctx, scope, batch))
}

func (part observedEvidence) QueryEvidence(ctx context.Context, scope EvidenceScope, query EvidenceQuery) (EvidencePage, error) {
	if err := ctx.Err(); err != nil {
		// The caller stopped waiting before the call: not a store answer.
		return EvidencePage{}, err
	}
	page, err := part.evidence.QueryEvidence(ctx, scope, query)
	return page, part.base.classify(err)
}

// QueryEvidenceSubjects keeps the store's reading of several subjects
// together, or reads them one by one where the store cannot.
func (part observedEvidence) QueryEvidenceSubjects(ctx context.Context, scope EvidenceScope, query EvidenceQuery, subjects []EvidenceSubject) ([]EvidencePage, error) {
	if err := ctx.Err(); err != nil {
		// The caller stopped waiting before the call: not a store answer.
		return nil, err
	}
	pages, err := QueryEvidenceSubjects(ctx, part.evidence, scope, query, subjects)
	return pages, part.base.classify(err)
}

func (part observedEvidence) EvidenceCheckpoint(ctx context.Context, scope EvidenceScope) (EvidenceCheckpoint, error) {
	if err := ctx.Err(); err != nil {
		// The caller stopped waiting before the call: not a store answer.
		return EvidenceCheckpoint{}, err
	}
	checkpoint, err := part.evidence.EvidenceCheckpoint(ctx, scope)
	return checkpoint, part.base.classify(err)
}

func (part observedEvidence) DeleteEvidence(ctx context.Context, scope EvidenceScope) error {
	if err := ctx.Err(); err != nil {
		// The caller stopped waiting before the call: not a store answer.
		return err
	}
	return part.base.classify(part.evidence.DeleteEvidence(ctx, scope))
}

type observedLookup struct {
	base   *observed
	lookup EvidenceScopeLookup
}

func (part observedLookup) FindEvidenceScope(ctx context.Context, orgID, clusterID, clusterUID string) (EvidenceScope, error) {
	if err := ctx.Err(); err != nil {
		// The caller stopped waiting before the call: not a store answer.
		return EvidenceScope{}, err
	}
	scope, err := part.lookup.FindEvidenceScope(ctx, orgID, clusterID, clusterUID)
	return scope, part.base.classify(err)
}
