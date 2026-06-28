package agent

import "sync"

// catalog_store.go — the optional pattern-catalog load/persist/read strategy
// seam.
//
// It lets a consumer (the enterprise boot path) supply an alternative way to
// load, persist, read and curate the log-pattern catalog without OSS knowing
// anything about how that strategy works. It mirrors the SetModeResolver /
// scheduler.SetOwnership / middleware.SetOrgResolver last-wins seams: one
// process-wide slot, registered once at boot, mutex-guarded so a boot-time
// registration is safely visible to the worker goroutine.
//
// It is deliberately tier-neutral — it names no instance, partition, replica,
// or HA concept. nil (the default) ⇒ the Catalog runs its current inline
// ReadBlob("patterns") / WriteBlob("patterns") / in-memory paths verbatim, so
// community and single-instance behaviour is byte-for-byte unchanged (one
// nil-check per routed call, no allocations, no goroutines). OSS installs
// nothing; we do NOT ship a default store — the nil branch IS the default.

// CatalogStore is the OPTIONAL load/persist/read strategy for the pattern
// catalog. nil (the default) ⇒ the Catalog runs its current inline whole-blob
// path unchanged. A consumer installs a strategy via SetCatalogStore; OSS
// installs nothing.
//
// Implementations must not retain the map arguments passed to Persist beyond
// the call (the Catalog passes its live maps under its own lock; the store is
// expected to serialize them synchronously, exactly as the inline path
// marshals under lock).
type CatalogStore interface {
	// Load returns the working-set this consumer wants the Catalog to hold in
	// memory after boot. Either returned map may be nil to mean "empty".
	Load() (patterns map[string]*Pattern, services map[string]*ServiceInfo, err error)

	// Persist writes the Catalog's current in-memory working set.
	Persist(patterns map[string]*Pattern, services map[string]*ServiceInfo) error

	// Snapshot returns the unified read view for the bulk/admin list reads
	// (Catalog.All / Catalog.AllServices) and the miner seed.
	Snapshot() (patterns []*Pattern, services map[string]ServiceInfo, err error)

	// Curate persists ONE operator edit (label/verdict/tags, delete,
	// mark-known, end/restart service grace) so it is visible to the read
	// view. It carries exactly the mutations the Catalog's own curation
	// methods perform inline.
	Curate(edit CatalogEdit) error
}

// CatalogEditKind discriminates which operator mutation a CatalogEdit carries.
// The set mirrors the Catalog curation method set one-for-one.
type CatalogEditKind string

const (
	// CatalogEditLabel carries Label(PatternID, Verdict, Tags).
	CatalogEditLabel CatalogEditKind = "label"
	// CatalogEditDelete carries Delete(PatternID).
	CatalogEditDelete CatalogEditKind = "delete"
	// CatalogEditMarkKnown carries MarkKnown(PatternID).
	CatalogEditMarkKnown CatalogEditKind = "mark_known"
	// CatalogEditEndServiceGrace carries EndServiceGrace(Service).
	CatalogEditEndServiceGrace CatalogEditKind = "end_service_grace"
	// CatalogEditRestartServiceGrace carries RestartServiceGrace(Service).
	CatalogEditRestartServiceGrace CatalogEditKind = "restart_service_grace"
)

// CatalogEdit is one operator mutation, modelled on the Catalog curation
// method set so a CatalogStore can apply it without importing the controllers.
// Kind selects which fields are meaningful:
//
//   - CatalogEditLabel              → PatternID, Verdict, Tags
//   - CatalogEditDelete             → PatternID
//   - CatalogEditMarkKnown          → PatternID
//   - CatalogEditEndServiceGrace    → Service
//   - CatalogEditRestartServiceGrace→ Service
type CatalogEdit struct {
	Kind CatalogEditKind
	// PatternID identifies the pattern for the label/delete/mark-known edits.
	PatternID string
	// Verdict is the operator verdict for a label edit ("" leaves it
	// unchanged, matching Catalog.Label).
	Verdict string
	// Tags are the operator tags for a label edit (nil leaves them unchanged,
	// matching Catalog.Label).
	Tags []string
	// Service identifies the service for the grace edits.
	Service string
}

// Process-wide single slot. A consumer registers a store at boot; the Catalog
// reads it once per routed call. Mutex-guarded so a boot-time registration is
// safely visible to the worker goroutine.
var (
	catalogStoreMu   sync.Mutex
	catalogStoreSlot CatalogStore
)

// SetCatalogStore installs the process-wide catalog load/persist/read
// strategy. Last-wins: a second call replaces the first. Passing nil clears
// the slot (back to the inline whole-blob path). Call at boot, before the
// worker starts and before LoadCatalog. OSS ships none, so the Catalog runs
// its current inline code unchanged. This is the entry point a consumer (e.g.
// the enterprise boot path) attaches to — mirror of scheduler.SetOwnership /
// SetModeResolver.
func SetCatalogStore(s CatalogStore) {
	catalogStoreMu.Lock()
	defer catalogStoreMu.Unlock()
	catalogStoreSlot = s
}

// catalogStore returns the installed store, or nil when none is set
// (community / single-instance inline whole-blob path).
func catalogStore() CatalogStore {
	catalogStoreMu.Lock()
	defer catalogStoreMu.Unlock()
	return catalogStoreSlot
}
