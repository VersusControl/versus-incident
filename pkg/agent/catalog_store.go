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
	//
	// Persist carries re-observation updates as well as fresh patterns: an
	// existing pattern's Service is REFRESHED in the working set whenever a real
	// attribution (an operator override or regex detection) resolves for it on a
	// tick (see Catalog.Upsert). A store MUST apply upsert semantics for the
	// Service field — overwrite the stored Service from the working-set value
	// rather than treating an existing pattern as immutable — or an operator's
	// "Reassign to service" never re-points an already-learned pattern in the
	// read view. Upsert stays off the store hot path (it never calls Curate);
	// this Service change rides Persist like every other working-set mutation.
	Persist(patterns map[string]*Pattern, services map[string]*ServiceInfo) error

	// Snapshot returns the unified read view for the bulk/admin list reads
	// (Catalog.All / Catalog.AllServices) and the miner seed.
	Snapshot() (patterns []*Pattern, services map[string]ServiceInfo, err error)

	// Curate persists ONE operator edit (label/verdict/tags, delete,
	// mark-known, end/restart service grace, or a whole-catalog reset) so it
	// is visible to the read view. It carries exactly the mutations the
	// Catalog's own curation methods perform inline.
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
	// CatalogEditRepointService carries RepointService(PatternID, Service): set
	// an EXISTING pattern's Service immediately — the retroactive half of an
	// operator "Reassign to service" correction. Unlike a Persist-carried
	// re-observation re-point (which only lands when a fresh matching log line
	// re-clusters the pattern via Upsert), this takes effect on the very next
	// read view, so a reassignment of a pattern that is not currently receiving
	// traffic is reflected immediately. It carries PatternID + Service (Service
	// is the NEW target, guaranteed by the caller to be a real, non-"_unknown"
	// service). A store MUST treat a missing PatternID as a no-op (the log
	// override match can be a message substring, not a pattern id) rather than
	// an error, exactly as the in-memory path does.
	CatalogEditRepointService CatalogEditKind = "repoint_service"
	// CatalogEditEndServiceGrace carries EndServiceGrace(Service).
	CatalogEditEndServiceGrace CatalogEditKind = "end_service_grace"
	// CatalogEditRestartServiceGrace carries RestartServiceGrace(Service).
	CatalogEditRestartServiceGrace CatalogEditKind = "restart_service_grace"
	// CatalogEditResetPatterns carries ResetPatterns(): wipe EVERY learned +
	// curated log pattern and persist the empty pattern set, LEAVING services
	// untouched. It carries no PatternID/Service — it targets the whole pattern
	// half, not one entry — so a store implementation empties only its pattern
	// read view.
	CatalogEditResetPatterns CatalogEditKind = "reset_patterns"
	// CatalogEditResetServices carries ResetServices(): wipe EVERY discovered +
	// manual service and persist the empty service set, LEAVING patterns
	// untouched. It carries no PatternID/Service — it targets the whole service
	// half, not one entry — so a store implementation empties only its service
	// read view.
	CatalogEditResetServices CatalogEditKind = "reset_services"
	// CatalogEditCreateService carries CreateService(Service): record an
	// operator-created (manual) service so it is selectable before any signal.
	CatalogEditCreateService CatalogEditKind = "create_service"
	// CatalogEditRenameService carries RenameService(Service, NewService): move
	// a manual service entry to a new name, preserving FirstSeen + manual flag.
	CatalogEditRenameService CatalogEditKind = "rename_service"
	// CatalogEditDelRepointService     → PatternID, Service (new target)
	//   - CatalogEditeteService carries DeleteService(Service): remove a manual
	// service entry.
	CatalogEditDeleteService CatalogEditKind = "delete_service"
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
//   - CatalogEditResetPatterns       → (no fields — clears every pattern)
//   - CatalogEditResetServices       → (no fields — clears every service)
//   - CatalogEditCreateService      → Service (manual service name)
//   - CatalogEditRenameService      → Service (old), NewService (new)
//   - CatalogEditDeleteService      → Service (manual service name)
type CatalogEdit struct {
	Kind CatalogEditKind
	// PatternID identifies the pattern for the label/delete/mark-known edits.
	PatternID string
	// Verdict is the operator verdict for a label edit — a tri-state pointer
	// matching Catalog.Label: nil leaves the stored verdict unchanged (a
	// tags-only edit), a non-nil pointer SETS it, and a non-nil pointer to the
	// empty string CLEARS it. A store MUST honour the &"" case (clear) — a
	// plain empty string can never clear, which is exactly the "Clear verdict"
	// no-op bug this seam change fixes.
	Verdict *string
	// Tags are the operator tags for a label edit (nil leaves them unchanged,
	// matching Catalog.Label).
	Tags []string
	// Service identifies the service for the grace and manual-service edits
	// (the OLD name for a rename) and the NEW target service for a
	// CatalogEditRepointService.
	Service string
	// NewService is the target name for a rename edit; unused otherwise.
	NewService string
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
