package agent

// catalog_pg_store_test.go — Two layers:
//
//  1. SQLi-safety / query-construction unit tests that run EVERYWHERE (no DB):
//     every query is a static constant that names only the fixed signal
//     tables and binds values as $N parameters — never fmt-interpolated.
//  2. A full catalog-lifecycle round-trip against a live Postgres, gated on
//     TEST_POSTGRES_DSN, that drives the PUBLIC Catalog API with the store
//     installed (Upsert/Persist/Snapshot, Label/MarkKnown/Delete, the samples
//     ring, RegisterService/grace, manual-service CRUD, and both resets).

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/stats"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// allCatalogQueries is every SQL string the Postgres catalog store issues.
var allCatalogQueries = []string{
	sqlCatalogLoadLogs, sqlCatalogUpsertRoot,
	sqlCatalogUpsertLog, sqlCatalogInsertServiceIfAbsent, sqlCatalogSnapshotLogs,
	sqlCatalogPageLogs, sqlCatalogCountLogs, sqlCatalogSnapshotServices,
	sqlCatalogPageServices, sqlCatalogCountServices, sqlCatalogLookupPattern,
	sqlCatalogLookupService,
	sqlCurateVerdict, sqlCurateTags, sqlCurateMarkKnown, sqlCurateRepointService,
	sqlCurateDelete, sqlCurateDeleteWritePatterns, sqlCurateResetPatterns,
	sqlCurateDeleteWriteServices, sqlCurateResetServices,
	sqlCurateEndGrace, sqlCurateRestartGrace, sqlCurateCreateService,
	sqlCurateDeleteService, sqlRenameSelectService, sqlRenameTombstoneOld,
	sqlRenameUpsertNewSvc,
}

// TestCatalogQueries_NoFormatVerbs proves no query carries a printf verb — the
// tables are Go constants embedded literally, never interpolated, so there is
// no dynamic-SQL surface (A03).
func TestCatalogQueries_NoFormatVerbs(t *testing.T) {
	for _, q := range allCatalogQueries {
		for _, verb := range []string{"%s", "%d", "%v", "%q", "%w"} {
			if strings.Contains(q, verb) {
				t.Fatalf("query contains format verb %q (dynamic SQL): %s", verb, q)
			}
		}
	}
}

// TestCatalogQueries_OnlyKnownTables proves every query touches ONLY the three
// signal tables — no stray table name, no enterprise vs_metrics/vs_traces.
func TestCatalogQueries_OnlyKnownTables(t *testing.T) {
	for _, q := range allCatalogQueries {
		lower := strings.ToLower(q)
		if strings.Contains(lower, "vs_metrics") || strings.Contains(lower, "vs_traces") {
			t.Fatalf("OSS catalog query must not name an enterprise table: %s", q)
		}
		if !strings.Contains(lower, "vs_patterns") &&
			!strings.Contains(lower, "vs_logs") &&
			!strings.Contains(lower, "vs_services") {
			t.Fatalf("query names no known signal table: %s", q)
		}
	}
}

// TestCatalogQueries_ParameterizedOrgScope proves every org-scoped statement
// binds org_id as a parameter ($1), so tenant isolation is a bound value and
// the id/service/name are never concatenated (A03 + tenant isolation).
func TestCatalogQueries_ParameterizedOrgScope(t *testing.T) {
	for _, q := range allCatalogQueries {
		if strings.Contains(q, "org_id") && !strings.Contains(q, "$1") {
			t.Fatalf("org-scoped query missing a bound $1 parameter: %s", q)
		}
	}
}

// TestNewPostgresCatalogStore_OrgNormalized proves an empty org is normalized
// to the default deployment org (never a blank org_id on the write path).
func TestNewPostgresCatalogStore_OrgNormalized(t *testing.T) {
	s := NewPostgresCatalogStore(nil, "", 0).(*pgCatalogStore)
	if s.orgID != storage.DefaultOrgID {
		t.Fatalf("orgID = %q, want %q", s.orgID, storage.DefaultOrgID)
	}
}

func TestNewPostgresCatalogStoreForScope_WriteFirst(t *testing.T) {
	s := NewPostgresCatalogStoreForScope(nil, tenancy.NewOrgScope("licensed", "default"), 0).(*pgCatalogStore)
	if s.orgID != "licensed" {
		t.Fatalf("orgID = %q, want licensed", s.orgID)
	}
	if got := s.orgScope.OrgIDs(); len(got) != 2 || got[0] != "licensed" || got[1] != "default" {
		t.Fatalf("org scope = %v, want [licensed default]", got)
	}
}

func TestCatalogLoadQuery_UsesRankedScopeAndInstancePartition(t *testing.T) {
	for _, fragment := range []string{
		"p.org_id = ANY($1)",
		"PARTITION BY p.id",
		"array_position($1::text[], p.org_id)",
		"PARTITION BY l.pattern_id, l.instance_index",
		"l.org_id = ANY($1)",
		"ON p.id = l.pattern_id",
		"reset_shadow = FALSE",
	} {
		if !strings.Contains(sqlCatalogLoadLogs, fragment) {
			t.Errorf("load query missing %q: %s", fragment, sqlCatalogLoadLogs)
		}
	}
	if strings.Contains(sqlCatalogLoadLogs, "l.org_id = $1") {
		t.Fatalf("load query still pins logs to the write org: %s", sqlCatalogLoadLogs)
	}
	if strings.Contains(sqlCatalogLoadLogs, "p.org_id = l.org_id") {
		t.Fatalf("load query still binds a learned partition to the curated root org: %s", sqlCatalogLoadLogs)
	}
}

func TestCatalogFleetQueries_RankEachInstancePartitionAcrossScope(t *testing.T) {
	for name, query := range map[string]string{
		"snapshot": sqlCatalogSnapshotLogs,
		"page":     sqlCatalogPageLogs,
		"count":    sqlCatalogCountLogs,
		"lookup":   sqlCatalogLookupPattern,
	} {
		for _, fragment := range []string{
			"PARTITION BY l.pattern_id, l.instance_index",
			"l.org_id = ANY($1)",
			"FROM chosen_logs",
			"p.reset_at IS NULL OR l.persisted_at > p.reset_at",
		} {
			if !strings.Contains(query, fragment) {
				t.Errorf("%s query missing %q: %s", name, fragment, query)
			}
		}
		if strings.Contains(query, "ON p.id = l.pattern_id AND p.org_id = l.org_id") {
			t.Errorf("%s query still unconditionally binds learned rows to the curated root org: %s", name, query)
		}
	}
}

func TestCatalogLoadQuery_ExcludesLowerScopePartitionsPredatingReset(t *testing.T) {
	for _, fragment := range []string{
		"l.org_id = p.org_id",
		"p.reset_at IS NULL",
		"l.persisted_at > p.reset_at",
	} {
		if !strings.Contains(sqlCatalogLoadLogs, fragment) {
			t.Errorf("load query missing reset cutoff %q: %s", fragment, sqlCatalogLoadLogs)
		}
	}
	if !strings.Contains(sqlCatalogUpsertLog, "persisted_at        = NOW()") {
		t.Fatalf("learned-row upsert does not refresh reset cutoff timestamp: %s", sqlCatalogUpsertLog)
	}
}

func TestCatalogResetQueries_ShadowLowerScopeWithWriteOrgTombstones(t *testing.T) {
	for name, queries := range map[string][2]string{
		"patterns": {sqlCurateDeleteWritePatterns, sqlCurateResetPatterns},
		"services": {sqlCurateDeleteWriteServices, sqlCurateResetServices},
	} {
		for _, fragment := range []string{"DELETE", "org_id = $1"} {
			if !strings.Contains(queries[0], fragment) {
				t.Errorf("%s reset delete missing %q: %s", name, fragment, queries[0])
			}
		}
		for _, fragment := range []string{"org_id = ANY($2)", "org_id <> $1", "deleted", "TRUE", "ON CONFLICT", "DO UPDATE"} {
			if !strings.Contains(queries[1], fragment) {
				t.Errorf("%s reset tombstone missing %q: %s", name, fragment, queries[1])
			}
		}
	}
	if !strings.Contains(sqlCurateResetServices, "first_seen") {
		t.Fatalf("service reset tombstone does not satisfy first_seen: %s", sqlCurateResetServices)
	}
}

func TestPGCatalog_ScopeDeduplicatesBeforeCountAndPage(t *testing.T) {
	_, db := newPGCatalog(t)
	now := time.Now().UTC()
	legacy := NewPostgresCatalogStore(db, storage.DefaultOrgID, 0)
	licensed := NewPostgresCatalogStore(db, "licensed", 0)

	if err := legacy.Persist(map[string]*Pattern{
		"shared": {ID: "shared", Template: "legacy shared", Count: 100, FirstSeen: now.Add(-2 * time.Hour), LastSeen: now},
		"legacy": {ID: "legacy", Template: "legacy only", Count: 7, FirstSeen: now.Add(-time.Hour), LastSeen: now},
	}, map[string]*ServiceInfo{
		"checkout": {FirstSeen: now.Add(-2 * time.Hour)},
		"legacy":   {FirstSeen: now.Add(-time.Hour)},
	}); err != nil {
		t.Fatalf("persist legacy catalog: %v", err)
	}
	if err := licensed.Persist(map[string]*Pattern{
		"shared": {ID: "shared", Template: "licensed shared", Count: 3, FirstSeen: now, LastSeen: now},
	}, map[string]*ServiceInfo{
		"checkout": {FirstSeen: now, Manual: true},
	}); err != nil {
		t.Fatalf("persist licensed catalog: %v", err)
	}

	scoped := NewPostgresCatalogStoreForScope(
		db, tenancy.NewOrgScope("licensed", storage.DefaultOrgID), 0,
	).(*pgCatalogStore)
	patterns, services, err := scoped.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(patterns) != 2 {
		t.Fatalf("Snapshot patterns = %d, want 2 after dedup", len(patterns))
	}
	shared := patternByID(patterns, "shared")
	if shared == nil || shared.OrgID != "licensed" || shared.Template != "licensed shared" || shared.Count != 3 {
		t.Fatalf("shared pattern = %#v, want licensed row", shared)
	}
	if legacyOnly := patternByID(patterns, "legacy"); legacyOnly == nil || legacyOnly.OrgID != storage.DefaultOrgID {
		t.Fatalf("legacy pattern = %#v, want visible default row", legacyOnly)
	}
	if len(services) != 2 || !services["checkout"].Manual || services["checkout"].OrgID != "licensed" {
		t.Fatalf("services = %#v, want two logical services with licensed checkout", services)
	}

	page, total, err := scoped.ListPatternsPage(CatalogPageOptions{Limit: 1})
	if err != nil {
		t.Fatalf("ListPatternsPage: %v", err)
	}
	if total != 2 || len(page) != 1 || page[0].ID != "legacy" {
		t.Fatalf("pattern page = %v total=%d, want [legacy] total=2", ids(page), total)
	}
	next, nextTotal, err := scoped.ListPatternsPage(CatalogPageOptions{Offset: 1, Limit: 1, Search: "shared"})
	if err != nil {
		t.Fatalf("ListPatternsPage search: %v", err)
	}
	if nextTotal != 1 || len(next) != 0 {
		t.Fatalf("searched offset page = %v total=%d, want empty total=1", ids(next), nextTotal)
	}
	servicePage, serviceTotal, err := scoped.ListServicesPage(CatalogPageOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListServicesPage: %v", err)
	}
	if serviceTotal != 2 || len(servicePage) != 2 {
		t.Fatalf("service page = %v total=%d, want two deduplicated services", svcNames(servicePage), serviceTotal)
	}

	loadedPatterns, loadedServices, err := scoped.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := loadedPatterns["shared"]; got == nil || got.OrgID != "licensed" || got.Template != "licensed shared" {
		t.Fatalf("loaded shared pattern = %#v, want licensed row", got)
	}
	if got := loadedPatterns["legacy"]; got == nil || got.OrgID != storage.DefaultOrgID || got.Template != "legacy only" {
		t.Fatalf("loaded legacy pattern = %#v, want default row", got)
	}
	if len(loadedServices) != 2 || loadedServices["legacy"].OrgID != storage.DefaultOrgID {
		t.Fatalf("loaded services = %#v, want scoped union", loadedServices)
	}
	lookup := any(scoped).(CatalogEntityLookup)
	if got, err := lookup.LookupPattern("shared"); err != nil || got == nil || got.OrgID != "licensed" {
		t.Fatalf("LookupPattern(shared) = %#v, %v", got, err)
	}
	if got, err := lookup.LookupService("legacy"); err != nil || got == nil || got.OrgID != storage.DefaultOrgID {
		t.Fatalf("LookupService(legacy) = %#v, %v", got, err)
	}
}

func TestEscapeCatalogLikePatternTreatsMetacharactersLiterally(t *testing.T) {
	if got, want := escapeCatalogLikePattern(`cpu_%\host`), `cpu\_\%\\host`; got != want {
		t.Fatalf("escapeCatalogLikePattern = %q, want %q", got, want)
	}
}

func TestPGCatalog_UpgradePersistPreservesLegacyCuration(t *testing.T) {
	_, db := newPGCatalog(t)
	now := time.Now().UTC()
	legacy := NewPostgresCatalogStore(db, storage.DefaultOrgID, 0).(*pgCatalogStore)
	if err := legacy.Persist(map[string]*Pattern{
		"known":   {ID: "known", Template: "known template", Count: 4, FirstSeen: now, LastSeen: now},
		"deleted": {ID: "deleted", Template: "deleted template", Count: 2, FirstSeen: now, LastSeen: now},
	}, nil); err != nil {
		t.Fatalf("seed legacy patterns: %v", err)
	}
	known := pgVerdictKnown
	if err := legacy.Curate(CatalogEdit{Kind: CatalogEditLabel, PatternID: "known", Verdict: &known, Tags: []string{"noise"}}); err != nil {
		t.Fatalf("curate legacy known pattern: %v", err)
	}
	if err := legacy.Curate(CatalogEdit{Kind: CatalogEditDelete, PatternID: "deleted"}); err != nil {
		t.Fatalf("delete legacy pattern: %v", err)
	}

	scoped := NewPostgresCatalogStoreForScope(
		db, tenancy.NewOrgScope("licensed", storage.DefaultOrgID), 0,
	).(*pgCatalogStore)
	patterns, services, err := scoped.Load()
	if err != nil {
		t.Fatalf("Load upgrade view: %v", err)
	}
	if err := scoped.Persist(patterns, services); err != nil {
		t.Fatalf("Persist upgrade view: %v", err)
	}

	var verdict string
	var tags []byte
	var deleted bool
	if err := db.QueryRow(`SELECT COALESCE(verdict, ''), tags, deleted FROM vs_patterns WHERE org_id = 'licensed' AND id = 'known'`).Scan(&verdict, &tags, &deleted); err != nil {
		t.Fatalf("read copied known curation: %v", err)
	}
	if verdict != pgVerdictKnown || string(tags) != `["noise"]` || deleted {
		t.Fatalf("copied known curation = verdict %q tags %s deleted %v", verdict, tags, deleted)
	}
	if err := db.QueryRow(`SELECT deleted FROM vs_patterns WHERE org_id = 'licensed' AND id = 'deleted'`).Scan(&deleted); err != nil {
		t.Fatalf("read copied deleted curation: %v", err)
	}
	if !deleted {
		t.Fatal("copied deleted pattern was revived during upgrade persist")
	}
}

func TestPGCatalog_UpgradePersistPreservesLegacyServiceCuration(t *testing.T) {
	_, db := newPGCatalog(t)
	legacyFirstSeen := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
	legacy := NewPostgresCatalogStore(db, storage.DefaultOrgID, 0).(*pgCatalogStore)
	if err := legacy.Persist(nil, map[string]*ServiceInfo{
		"deleted-api": {Manual: true, FirstSeen: legacyFirstSeen},
	}); err != nil {
		t.Fatalf("seed legacy service: %v", err)
	}
	if err := legacy.Curate(CatalogEdit{Kind: CatalogEditDeleteService, Service: "deleted-api"}); err != nil {
		t.Fatalf("delete legacy service: %v", err)
	}

	scoped := NewPostgresCatalogStoreForScope(
		db, tenancy.NewOrgScope("licensed", storage.DefaultOrgID), 0,
	).(*pgCatalogStore)
	if err := scoped.Persist(nil, map[string]*ServiceInfo{
		"deleted-api": {FirstSeen: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("persist discovered service during upgrade: %v", err)
	}

	var (
		manual      bool
		firstSeen   time.Time
		deleted     bool
		resetShadow bool
	)
	if err := db.QueryRow(`
		SELECT manual, first_seen, deleted, reset_shadow
		FROM vs_services WHERE org_id = 'licensed' AND name = 'deleted-api'`,
	).Scan(&manual, &firstSeen, &deleted, &resetShadow); err != nil {
		t.Fatalf("read copied service curation: %v", err)
	}
	if !manual || !firstSeen.Equal(legacyFirstSeen) || !deleted || resetShadow {
		t.Fatalf("copied service curation = manual %v first_seen %v deleted %v reset_shadow %v, want true %v true false",
			manual, firstSeen, deleted, resetShadow, legacyFirstSeen)
	}
	_, services, err := scoped.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, ok := services["deleted-api"]; ok {
		t.Fatalf("legacy explicitly deleted service resurfaced after upgrade: %#v", services)
	}
}

func TestPGCatalog_RollingUpgradeKeepsEachScopedLearnedPartition(t *testing.T) {
	_, db := newPGCatalog(t)
	now := time.Now().UTC()
	legacyZero := NewPostgresCatalogStore(db, storage.DefaultOrgID, 0)
	if err := legacyZero.Persist(map[string]*Pattern{
		"p-ha": {
			ID: "p-ha", Template: "instance zero", Count: 100,
			BaselineFrequency: 10, FirstSeen: now.Add(-2 * time.Hour), LastSeen: now,
		},
	}, nil); err != nil {
		t.Fatalf("seed legacy instance zero: %v", err)
	}
	legacyOne := NewPostgresCatalogStore(db, storage.DefaultOrgID, 1)
	if err := legacyOne.Persist(map[string]*Pattern{
		"p-ha": {
			ID: "p-ha", Template: "instance one", Count: 250,
			BaselineFrequency: 25, FirstSeen: now.Add(-time.Hour), LastSeen: now,
		},
	}, nil); err != nil {
		t.Fatalf("seed legacy instance one: %v", err)
	}

	scope := tenancy.NewOrgScope("licensed", storage.DefaultOrgID)
	upgradedZero := NewPostgresCatalogStoreForScope(db, scope, 0)
	zeroPatterns, _, err := upgradedZero.Load()
	if err != nil {
		t.Fatalf("load instance zero during upgrade: %v", err)
	}
	if got := zeroPatterns["p-ha"]; got == nil || got.Count != 100 || got.Template != "instance zero" {
		t.Fatalf("instance zero Load = %#v, want only its legacy partition", got)
	}
	if err := upgradedZero.Persist(zeroPatterns, nil); err != nil {
		t.Fatalf("persist upgraded instance zero: %v", err)
	}

	upgradedOne := NewPostgresCatalogStoreForScope(db, scope, 1).(*pgCatalogStore)
	onePatterns, _, err := upgradedOne.Load()
	if err != nil {
		t.Fatalf("load instance one after write-org root exists: %v", err)
	}
	if got := onePatterns["p-ha"]; got == nil || got.OrgID != "licensed" || got.Count != 250 || got.Template != "instance one" {
		t.Fatalf("instance one Load = %#v, want licensed curation with only its legacy partition", got)
	}

	snapshot, _, err := upgradedOne.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	got := patternByID(snapshot, "p-ha")
	if got == nil || got.Count != 350 || got.BaselineFrequency != 35 || got.Template != "instance zero" {
		t.Fatalf("fleet Snapshot = %#v, want count=350 baseline=35 and lowest-index template", got)
	}
	page, total, err := upgradedOne.ListPatternsPage(CatalogPageOptions{Search: "p-ha", Limit: 10})
	if err != nil {
		t.Fatalf("ListPatternsPage: %v", err)
	}
	if total != 1 || len(page) != 1 || page[0].Count != 350 {
		t.Fatalf("fleet page = %#v total=%d, want one deduplicated count-350 row", page, total)
	}
	lookup, err := upgradedOne.LookupPattern("p-ha")
	if err != nil {
		t.Fatalf("LookupPattern: %v", err)
	}
	if lookup == nil || lookup.Count != 350 {
		t.Fatalf("LookupPattern = %#v, want fleet count 350", lookup)
	}
}

func TestPGCatalog_UnionResetsShadowLegacyRows(t *testing.T) {
	_, db := newPGCatalog(t)
	now := time.Now().UTC()
	legacy := NewPostgresCatalogStore(db, storage.DefaultOrgID, 0)
	if err := legacy.Persist(map[string]*Pattern{
		"legacy": {ID: "legacy", Template: "legacy history", Count: 9, FirstSeen: now, LastSeen: now},
	}, map[string]*ServiceInfo{
		"legacy-service": {FirstSeen: now},
	}); err != nil {
		t.Fatalf("seed legacy catalog: %v", err)
	}
	licensed := NewPostgresCatalogStore(db, "licensed", 0)
	if err := licensed.Persist(map[string]*Pattern{
		"legacy": {ID: "legacy", Template: "licensed history", Count: 3, FirstSeen: now, LastSeen: now},
	}, map[string]*ServiceInfo{
		"legacy-service": {FirstSeen: now},
	}); err != nil {
		t.Fatalf("seed overlapping licensed catalog: %v", err)
	}
	scoped := NewPostgresCatalogStoreForScope(db, tenancy.NewOrgScope("licensed", storage.DefaultOrgID), 0)
	SetCatalogStore(scoped)
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if len(cat.All()) != 1 || len(cat.AllServices()) != 1 {
		t.Fatalf("upgrade view patterns=%d services=%d, want legacy rows visible", len(cat.All()), len(cat.AllServices()))
	}
	if _, err := cat.ResetPatterns(); err != nil {
		t.Fatalf("ResetPatterns: %v", err)
	}
	if len(cat.All()) != 0 {
		t.Fatalf("patterns resurfaced after reset: %#v", cat.All())
	}
	if _, err := cat.ResetServices(); err != nil {
		t.Fatalf("ResetServices: %v", err)
	}
	if len(cat.AllServices()) != 0 {
		t.Fatalf("services resurfaced after reset: %#v", cat.AllServices())
	}

	reloaded := NewPostgresCatalogStoreForScope(db, tenancy.NewOrgScope("licensed", storage.DefaultOrgID), 0)
	patterns, services, err := reloaded.Load()
	if err != nil {
		t.Fatalf("restart Load: %v", err)
	}
	if len(patterns) != 0 || len(services) != 0 {
		t.Fatalf("restart Load patterns=%#v services=%#v, want reset shadows", patterns, services)
	}
	snapshot, snapshotServices, err := reloaded.Snapshot()
	if err != nil {
		t.Fatalf("restart Snapshot: %v", err)
	}
	if len(snapshot) != 0 || len(snapshotServices) != 0 {
		t.Fatalf("restart Snapshot patterns=%#v services=%#v, want reset shadows", snapshot, snapshotServices)
	}
	pager := reloaded.(CatalogPager)
	if page, total, err := pager.ListPatternsPage(CatalogPageOptions{Limit: 10}); err != nil || total != 0 || len(page) != 0 {
		t.Fatalf("pattern page after reset = %#v total=%d err=%v", page, total, err)
	}
	if page, total, err := pager.ListServicesPage(CatalogPageOptions{Limit: 10}); err != nil || total != 0 || len(page) != 0 {
		t.Fatalf("service page after reset = %#v total=%d err=%v", page, total, err)
	}
	lookup := reloaded.(CatalogEntityLookup)
	if got, err := lookup.LookupPattern("legacy"); err != nil || got != nil {
		t.Fatalf("LookupPattern after reset = %#v, %v", got, err)
	}
	if got, err := lookup.LookupService("legacy-service"); err != nil || got != nil {
		t.Fatalf("LookupService after reset = %#v, %v", got, err)
	}
}

func TestPGCatalog_ResetShadowsReviveWhenRelearnedAfterRestart(t *testing.T) {
	_, db := newPGCatalog(t)
	now := time.Now().UTC()
	legacy := NewPostgresCatalogStore(db, storage.DefaultOrgID, 0)
	if err := legacy.Persist(map[string]*Pattern{
		"legacy": {ID: "legacy", Template: "legacy history", Count: 9, FirstSeen: now, LastSeen: now},
	}, map[string]*ServiceInfo{
		"legacy-service": {FirstSeen: now},
	}); err != nil {
		t.Fatalf("seed legacy catalog: %v", err)
	}
	scope := tenancy.NewOrgScope("licensed", storage.DefaultOrgID)
	beforeRestart := NewPostgresCatalogStoreForScope(db, scope, 0)
	if err := beforeRestart.Curate(CatalogEdit{Kind: CatalogEditResetPatterns}); err != nil {
		t.Fatalf("reset patterns: %v", err)
	}
	if err := beforeRestart.Curate(CatalogEdit{Kind: CatalogEditResetServices}); err != nil {
		t.Fatalf("reset services: %v", err)
	}

	afterRestart := NewPostgresCatalogStoreForScope(db, scope, 0).(*pgCatalogStore)
	if err := afterRestart.Persist(map[string]*Pattern{
		"legacy": {ID: "legacy", Template: "relearned history", Count: 1, FirstSeen: now, LastSeen: now},
	}, map[string]*ServiceInfo{
		"legacy-service": {FirstSeen: now},
	}); err != nil {
		t.Fatalf("persist relearned catalog: %v", err)
	}
	patterns, services, err := afterRestart.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := patternByID(patterns, "legacy"); got == nil || got.OrgID != "licensed" || got.Template != "relearned history" {
		t.Fatalf("relearned pattern = %#v, want visible licensed row", got)
	}
	if got, ok := services["legacy-service"]; !ok || got.OrgID != "licensed" {
		t.Fatalf("relearned services = %#v, want visible licensed service", services)
	}

	if err := afterRestart.Curate(CatalogEdit{Kind: CatalogEditDelete, PatternID: "legacy"}); err != nil {
		t.Fatalf("delete relearned pattern: %v", err)
	}
	if err := afterRestart.Curate(CatalogEdit{Kind: CatalogEditDeleteService, Service: "legacy-service"}); err != nil {
		t.Fatalf("delete relearned service: %v", err)
	}
	if err := afterRestart.Persist(map[string]*Pattern{
		"legacy": {ID: "legacy", Template: "observed after delete", Count: 2, FirstSeen: now, LastSeen: now},
	}, map[string]*ServiceInfo{
		"legacy-service": {FirstSeen: now},
	}); err != nil {
		t.Fatalf("persist after explicit deletes: %v", err)
	}
	patterns, services, err = afterRestart.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot after explicit deletes: %v", err)
	}
	if patternByID(patterns, "legacy") != nil {
		t.Fatalf("explicitly deleted pattern revived: %#v", patterns)
	}
	if _, ok := services["legacy-service"]; ok {
		t.Fatalf("explicitly deleted service revived: %#v", services)
	}
}

func TestPGCatalog_ResetExcludesStaleLowerScopeHAPartitions(t *testing.T) {
	_, db := newPGCatalog(t)
	now := time.Now().UTC()
	legacyZero := NewPostgresCatalogStore(db, storage.DefaultOrgID, 0)
	legacyOne := NewPostgresCatalogStore(db, storage.DefaultOrgID, 1)
	if err := legacyZero.Persist(map[string]*Pattern{
		"p-ha": {ID: "p-ha", Template: "legacy zero", Count: 100, FirstSeen: now, LastSeen: now},
	}, nil); err != nil {
		t.Fatalf("seed legacy instance zero: %v", err)
	}
	if err := legacyOne.Persist(map[string]*Pattern{
		"p-ha": {ID: "p-ha", Template: "legacy one", Count: 250, FirstSeen: now, LastSeen: now},
	}, nil); err != nil {
		t.Fatalf("seed legacy instance one: %v", err)
	}

	scope := tenancy.NewOrgScope("licensed", storage.DefaultOrgID)
	licensedZero := NewPostgresCatalogStoreForScope(db, scope, 0)
	if err := licensedZero.Curate(CatalogEdit{Kind: CatalogEditResetPatterns}); err != nil {
		t.Fatalf("reset patterns: %v", err)
	}
	if err := licensedZero.Persist(map[string]*Pattern{
		"p-ha": {ID: "p-ha", Template: "relearned zero", Count: 4, FirstSeen: now, LastSeen: now},
	}, nil); err != nil {
		t.Fatalf("relearn instance zero: %v", err)
	}

	snapshot, _, err := licensedZero.Snapshot()
	if err != nil {
		t.Fatalf("snapshot after instance zero relearns: %v", err)
	}
	if got := patternByID(snapshot, "p-ha"); got == nil || got.Count != 4 || got.Template != "relearned zero" {
		t.Fatalf("snapshot after reset = %#v, want only relearned instance-zero count 4", got)
	}
	page, total, err := licensedZero.(CatalogPager).ListPatternsPage(CatalogPageOptions{Search: "p-ha", Limit: 10})
	if err != nil {
		t.Fatalf("page after instance zero relearns: %v", err)
	}
	if total != 1 || len(page) != 1 || page[0].Count != 4 {
		t.Fatalf("page after reset = %#v total=%d, want one count-4 row", page, total)
	}
	lookup, err := licensedZero.(CatalogEntityLookup).LookupPattern("p-ha")
	if err != nil {
		t.Fatalf("lookup after instance zero relearns: %v", err)
	}
	if lookup == nil || lookup.Count != 4 {
		t.Fatalf("lookup after reset = %#v, want count 4", lookup)
	}

	licensedOne := NewPostgresCatalogStoreForScope(db, scope, 1)
	patterns, _, err := licensedOne.Load()
	if err != nil {
		t.Fatalf("instance one Load after reset: %v", err)
	}
	if got := patterns["p-ha"]; got != nil {
		t.Fatalf("instance one reloaded stale lower-scope partition: %#v", got)
	}

	if err := licensedOne.Persist(map[string]*Pattern{
		"p-ha": {ID: "p-ha", Template: "relearned one", Count: 7, FirstSeen: now, LastSeen: now},
	}, nil); err != nil {
		t.Fatalf("relearn instance one: %v", err)
	}
	snapshot, _, err = licensedOne.Snapshot()
	if err != nil {
		t.Fatalf("snapshot after instance one relearns: %v", err)
	}
	if got := patternByID(snapshot, "p-ha"); got == nil || got.Count != 11 {
		t.Fatalf("snapshot after both instances relearn = %#v, want fleet count 11", got)
	}
}

// ---------------------------------------------------------------------------
// Live-Postgres lifecycle round-trip (gated on TEST_POSTGRES_DSN)
// ---------------------------------------------------------------------------

func newPGCatalog(t *testing.T) (*Catalog, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}
	store, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	acc, ok := store.(storage.SQLAccessor)
	if !ok {
		t.Fatal("postgres provider must implement storage.SQLAccessor")
	}
	db := acc.DB()
	// Fresh slate: the typed signal tables only (CASCADE clears vs_logs too).
	if _, err := db.Exec(`TRUNCATE TABLE vs_patterns, vs_logs, vs_services CASCADE`); err != nil {
		t.Fatalf("truncate signal tables: %v", err)
	}

	SetCatalogStore(NewPostgresCatalogStore(db, storage.DefaultOrgID, 0))
	t.Cleanup(func() {
		SetCatalogStore(nil)
		_ = store.Close()
	})

	cat, err := LoadCatalog(store)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	// Wire the same baseline fold the agent worker installs at boot
	// (worker.go), so Upsert folds a real per-second rate: the global
	// EWMA + variance, the 24 hour-of-day seasonal buckets, and the
	// cumulative arithmetic average. Without this the catalog uses the
	// legacy mean-only fold and baseline_avg/variance/seasonal never move.
	cat.SetBaselineFold(resolveSpikeParams(config.AgentCatalogConfig{}, 30).fold())
	return cat, db
}

func patternByID(ps []*Pattern, id string) *Pattern {
	for _, p := range ps {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// TestPGCatalog_PatternLifecycle exercises the log-pattern half end to end.
func TestPGCatalog_PatternLifecycle(t *testing.T) {
	cat, db := newPGCatalog(t)

	// Learn two patterns across two ticks, with a sample on one.
	cat.Upsert("p1", "template one", "src-a", 3, 0.2, "default", "checkout")
	cat.Upsert("p2", "template two", "src-b", 1, 0.2, "rule-x", "")
	cat.RecordSample("p1", "GET /checkout 500 error", nil)
	cat.Upsert("p1", "template one", "src-a", 2, 0.2, "default", "checkout")
	if err := cat.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	// Snapshot (fleet read) via All(): both patterns, summed counts, sample.
	all := cat.All()
	if len(all) != 2 {
		t.Fatalf("All() len = %d, want 2", len(all))
	}
	p1 := patternByID(all, "p1")
	if p1 == nil || p1.Count != 5 {
		t.Fatalf("p1 count = %v, want 5", p1)
	}
	if p1.Service != "checkout" {
		t.Fatalf("p1 service = %q, want checkout", p1.Service)
	}
	if len(p1.Samples) != 1 || p1.Samples[0] != "GET /checkout 500 error" {
		t.Fatalf("p1 samples = %v, want one redacted sample", p1.Samples)
	}

	// instance_index defaults to 0 on the single-instance OSS write path.
	var idx int
	if err := db.QueryRow(
		`SELECT instance_index FROM vs_logs WHERE org_id=$1 AND pattern_id='p1'`,
		storage.DefaultOrgID,
	).Scan(&idx); err != nil {
		t.Fatalf("read instance_index: %v", err)
	}
	if idx != 0 {
		t.Fatalf("instance_index = %d, want 0", idx)
	}

	// Label: set verdict + tags (curated root columns).
	known := "known"
	if !cat.Label("p2", &known, []string{"noise"}) {
		t.Fatal("Label p2 returned false")
	}
	p2 := patternByID(cat.All(), "p2")
	if p2 == nil || p2.Verdict != "known" || len(p2.Tags) != 1 || p2.Tags[0] != "noise" {
		t.Fatalf("p2 after label = %+v", p2)
	}

	// Clear verdict (tri-state &""): p2 verdict blanks fleet-wide.
	clear := ""
	if !cat.Label("p2", &clear, nil) {
		t.Fatal("Label clear returned false")
	}
	if got := patternByID(cat.All(), "p2"); got.Verdict != "" {
		t.Fatalf("p2 verdict after clear = %q, want empty", got.Verdict)
	}

	// MarkKnown twice: the second is a churn-cached no-op (still verdict known).
	if !cat.MarkKnown("p1") {
		t.Fatal("MarkKnown p1 returned false")
	}
	_ = cat.MarkKnown("p1") // no-op, must not error
	if got := patternByID(cat.All(), "p1"); got.Verdict != "known" {
		t.Fatalf("p1 verdict = %q, want known", got.Verdict)
	}

	// Delete (tombstone): p2 disappears from the read view.
	if !cat.Delete("p2") {
		t.Fatal("Delete p2 returned false")
	}
	if patternByID(cat.All(), "p2") != nil {
		t.Fatal("p2 still present after delete")
	}
	if len(cat.All()) != 1 {
		t.Fatalf("All() len after delete = %d, want 1", len(cat.All()))
	}

	// ResetPatterns wipes the log half (FK cascade clears vs_logs).
	n, err := cat.ResetPatterns()
	if err != nil {
		t.Fatalf("ResetPatterns: %v", err)
	}
	if n != 1 {
		t.Fatalf("ResetPatterns removed %d, want 1 (the pre-reset visible count)", n)
	}
	if len(cat.All()) != 0 {
		t.Fatalf("All() after reset = %d, want 0", len(cat.All()))
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM vs_logs WHERE org_id=$1`, storage.DefaultOrgID).Scan(&rows); err != nil {
		t.Fatalf("count vs_logs: %v", err)
	}
	if rows != 0 {
		t.Fatalf("vs_logs rows after reset = %d, want 0 (FK cascade)", rows)
	}
}

// TestPGCatalog_ServiceLifecycle exercises the discovered/manual service half.
func TestPGCatalog_ServiceLifecycle(t *testing.T) {
	cat, _ := newPGCatalog(t)

	// Discovery rides Persist.
	if !cat.RegisterService("payments") {
		t.Fatal("RegisterService payments returned false (want newly registered)")
	}
	if cat.RegisterService("payments") {
		t.Fatal("second RegisterService payments returned true (want already-known)")
	}
	if err := cat.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if _, ok := cat.AllServices()["payments"]; !ok {
		t.Fatal("payments not in AllServices after persist")
	}

	// Grace: in window, then end it. IsServiceInGrace reads the in-memory
	// working set; grace edits route through Curate (DB) and are
	// eventually-consistent via the read view / next Load — the shared
	// CatalogStore contract (the enterprise partition store behaves the same).
	// So assert grace-in-window on the in-memory anchor, and grace-ended on the
	// fleet read view (AllServices → Snapshot → DB).
	if !cat.IsServiceInGrace("payments", time.Hour) {
		t.Fatal("payments should be within a 1h grace window")
	}
	if !cat.EndServiceGrace("payments") {
		t.Fatal("EndServiceGrace payments returned false")
	}
	ended := cat.AllServices()["payments"]
	if time.Now().UTC().Before(ended.FirstSeen.Add(time.Hour)) {
		t.Fatalf("payments grace anchor %v still within a 1h window after EndServiceGrace", ended.FirstSeen)
	}

	// Manual create — selectable before any signal, origin preserved.
	if err := cat.CreateService("billing"); err != nil {
		t.Fatalf("CreateService billing: %v", err)
	}
	if info, ok := cat.AllServices()["billing"]; !ok || !info.Manual {
		t.Fatalf("billing manual service missing/not manual: %+v ok=%v", info, ok)
	}

	// Rename manual service: old gone, new present + still manual.
	if err := cat.RenameService("billing", "billing-v2"); err != nil {
		t.Fatalf("RenameService: %v", err)
	}
	svcs := cat.AllServices()
	if _, ok := svcs["billing"]; ok {
		t.Fatal("old service name still present after rename")
	}
	if info, ok := svcs["billing-v2"]; !ok || !info.Manual {
		t.Fatalf("renamed service missing/not manual: %+v ok=%v", info, ok)
	}

	// Delete manual service (tombstone) — dropped from the read view.
	if !cat.DeleteService("billing-v2") {
		t.Fatal("DeleteService returned false")
	}
	if _, ok := cat.AllServices()["billing-v2"]; ok {
		t.Fatal("deleted service still present")
	}

	// ResetServices wipes them all, leaving patterns untouched.
	if _, err := cat.ResetServices(); err != nil {
		t.Fatalf("ResetServices: %v", err)
	}
	if len(cat.AllServices()) != 0 {
		t.Fatalf("AllServices after reset = %d, want 0", len(cat.AllServices()))
	}
}

// TestPGCatalog_ReloadRoundTrip proves persisted learned + curated state
// survives a fresh Load (a process restart) — the boot read is the same view.
func TestPGCatalog_ReloadRoundTrip(t *testing.T) {
	cat, db := newPGCatalog(t)

	cat.Upsert("keep", "kept template", "src", 4, 0.2, "default", "orders")
	if err := cat.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	known := "known"
	cat.Label("keep", &known, []string{"routine"})

	// Fresh store + Load against the same DB simulates a restart.
	reloaded := NewPostgresCatalogStore(db, storage.DefaultOrgID, 0)
	patterns, _, err := reloaded.Load()
	if err != nil {
		t.Fatalf("reload Load: %v", err)
	}
	got := patterns["keep"]
	if got == nil {
		t.Fatal("pattern 'keep' missing after reload")
	}
	if got.Count != 4 || got.Service != "orders" || got.Verdict != "known" {
		t.Fatalf("reloaded pattern = %+v, want count=4 service=orders verdict=known", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "routine" {
		t.Fatalf("reloaded tags = %v, want [routine]", got.Tags)
	}
	// The arithmetic-average baseline is folded during Upsert and must survive
	// the round-trip through the new vs_logs.baseline_avg column. An unset
	// per-pattern mode round-trips as '' (inherit the config default).
	if got.BaselineAvg <= 0 {
		t.Fatalf("reloaded baseline_avg = %v, want > 0 (folded during learn)", got.BaselineAvg)
	}
	if got.SpikeBaselineMode != "" {
		t.Fatalf("reloaded spike_baseline_mode = %q, want empty (inherit config default)", got.SpikeBaselineMode)
	}
}

// TestPGCatalog_BaselineModeRoundTrip proves every spike-baseline column added
// by migration 007 — baseline_avg (DOUBLE) and spike_baseline_mode (TEXT) —
// together with the pre-existing baseline_variance and the 24 hour-of-day
// seasonal buckets, survives a write→reload cycle through the typed vs_logs
// row unchanged, in EACH of the three per-pattern modes. It constructs the
// patterns directly (not via the learner) so the asserted values are exact and
// the mode string is the one persisted, isolating the column round-trip.
func TestPGCatalog_BaselineModeRoundTrip(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}
	store, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	acc, ok := store.(storage.SQLAccessor)
	if !ok {
		t.Fatal("postgres provider must implement storage.SQLAccessor")
	}
	db := acc.DB()
	if _, err := db.Exec(`TRUNCATE TABLE vs_patterns, vs_logs, vs_services CASCADE`); err != nil {
		t.Fatalf("truncate signal tables: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// One pattern per mode. Each carries a distinct global stat, a cumulative
	// average, and a populated hour-of-day seasonal bucket, so a lost/rewritten
	// column shows up as a mismatched read-back.
	seasonal := make([]stats.EWMA, stats.HoursPerDay)
	seasonal[2] = stats.EWMA{Mean: 44, Variance: 6.25, Count: 100}
	seasonal[9] = stats.EWMA{Mean: 12.5, Variance: 1.5, Count: 37}
	now := time.Now().UTC()
	want := map[string]struct {
		mode     string
		freq     float64
		variance float64
		avg      float64
	}{
		"p-default":     {"default", 10, 4, 20},
		"p-average":     {"average", 8.5, 2.25, 17.75},
		"p-time-of-day": {"time_of_day", 44, 6.25, 30.5},
	}
	patterns := make(map[string]*Pattern, len(want))
	for id, w := range want {
		patterns[id] = &Pattern{
			ID:                id,
			OrgID:             storage.DefaultOrgID,
			Template:          "boom <*>",
			Count:             200,
			BaselineFrequency: w.freq,
			BaselineVariance:  w.variance,
			BaselineAvg:       w.avg,
			SpikeBaselineMode: w.mode,
			Seasonal:          seasonal,
			FirstSeen:         now,
			LastSeen:          now,
		}
	}

	cs := NewPostgresCatalogStore(db, storage.DefaultOrgID, 0)
	if err := cs.Persist(patterns, nil); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	// Fresh store + Load simulates a process restart: the columns are read
	// straight back from vs_logs, not served from an in-memory cache.
	reloaded := NewPostgresCatalogStore(db, storage.DefaultOrgID, 0)
	got, _, err := reloaded.Load()
	if err != nil {
		t.Fatalf("reload Load: %v", err)
	}
	for id, w := range want {
		p := got[id]
		if p == nil {
			t.Fatalf("pattern %q missing after reload", id)
		}
		if p.SpikeBaselineMode != w.mode {
			t.Fatalf("%s spike_baseline_mode = %q, want %q", id, p.SpikeBaselineMode, w.mode)
		}
		if p.BaselineAvg != w.avg {
			t.Fatalf("%s baseline_avg = %v, want %v", id, p.BaselineAvg, w.avg)
		}
		if p.BaselineFrequency != w.freq {
			t.Fatalf("%s baseline_frequency = %v, want %v", id, p.BaselineFrequency, w.freq)
		}
		if p.BaselineVariance != w.variance {
			t.Fatalf("%s baseline_variance = %v, want %v", id, p.BaselineVariance, w.variance)
		}
		// The 24 seasonal buckets survive intact (values + counts).
		if len(p.Seasonal) != stats.HoursPerDay {
			t.Fatalf("%s seasonal len = %d, want %d", id, len(p.Seasonal), stats.HoursPerDay)
		}
		if p.Seasonal[2] != seasonal[2] {
			t.Fatalf("%s seasonal[2] = %+v, want %+v", id, p.Seasonal[2], seasonal[2])
		}
		if p.Seasonal[9] != seasonal[9] {
			t.Fatalf("%s seasonal[9] = %+v, want %+v", id, p.Seasonal[9], seasonal[9])
		}
	}
}

// redactScrubber redacts a fixed secret token so the storage-boundary re-scrub
// is observable (mirrors the enterprise store_pg_test.go scrubber).
type redactScrubber struct{ secret string }

func (r redactScrubber) Scrub(s string) string {
	return strings.ReplaceAll(s, r.secret, "<REDACTED>")
}

// TestPGCatalog_RedactionAtStorageBoundary proves a secret planted
// directly in a pattern's samples ring — bypassing the learn-boundary scrub —
// is re-scrubbed at the STORAGE boundary before it reaches vs_logs, so no
// secret ever lands in a signal table. It mirrors the enterprise
// TestPGBaseline_RedactionAtStorageBoundary so both signal-table write paths
// are defence-in-depth-equal.
func TestPGCatalog_RedactionAtStorageBoundary(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}
	store, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	acc, ok := store.(storage.SQLAccessor)
	if !ok {
		t.Fatal("postgres provider must implement storage.SQLAccessor")
	}
	db := acc.DB()
	if _, err := db.Exec(`TRUNCATE TABLE vs_patterns, vs_logs, vs_services CASCADE`); err != nil {
		t.Fatalf("truncate signal tables: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cs := NewPostgresCatalogStore(db, storage.DefaultOrgID, 0).(*pgCatalogStore)
	cs.SetSampleScrubber(redactScrubber{secret: "hunter2"})

	// Plant a raw secret directly in the ring (NOT via RecordSample), so the
	// only thing that can scrub it is the storage-boundary re-scrub on Persist.
	patterns := map[string]*Pattern{
		"p-secret": {
			ID:        "p-secret",
			OrgID:     storage.DefaultOrgID,
			Template:  "boom <*>",
			Count:     1,
			FirstSeen: time.Now().UTC(),
			LastSeen:  time.Now().UTC(),
			Samples:   []string{"password=hunter2 boom 500"},
		},
	}
	if err := cs.Persist(patterns, nil); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	// Belt: the raw samples column bytes carry no secret.
	var raw string
	if err := db.QueryRow(
		`SELECT samples::text FROM vs_logs WHERE org_id=$1 AND pattern_id='p-secret'`,
		storage.DefaultOrgID,
	).Scan(&raw); err != nil {
		t.Fatalf("read samples column: %v", err)
	}
	if strings.Contains(raw, "hunter2") {
		t.Fatalf("secret present in the vs_logs.samples column: %q", raw)
	}
	if !strings.Contains(raw, "<REDACTED>") {
		t.Fatalf("expected the redacted placeholder in the persisted ring, got: %q", raw)
	}

	// And the read view (Snapshot) surfaces only the scrubbed sample.
	snap, _, err := cs.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	got := patternByID(snap, "p-secret")
	if got == nil {
		t.Fatal("pattern 'p-secret' missing from snapshot")
	}
	if len(got.Samples) != 1 || strings.Contains(got.Samples[0], "hunter2") {
		t.Fatalf("secret survived the storage-boundary re-scrub: %v", got.Samples)
	}
}

// TestPGCatalog_SnapshotLoadBaselineParity guards the fleet-read/per-pattern-read
// column-parity bug: for a single-instance pattern (instance_index = 0, the
// common OSS case) the fleet-wide Snapshot (backing the list/peek) must return
// the exact same learned baseline fields as the per-partition Load (backing the
// detail page) — baseline_frequency, baseline_variance, baseline_avg,
// spike_baseline_mode, and the hour-of-day seasonal buckets. A regression that
// drops any of these columns from the Snapshot query/scan (leaving the list with
// an empty seasonal baseline or a zeroed average/variance while the detail is
// correct) fails here.
func TestPGCatalog_SnapshotLoadBaselineParity(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}
	store, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	acc, ok := store.(storage.SQLAccessor)
	if !ok {
		t.Fatal("postgres provider must implement storage.SQLAccessor")
	}
	db := acc.DB()
	if _, err := db.Exec(`TRUNCATE TABLE vs_patterns, vs_logs, vs_services CASCADE`); err != nil {
		t.Fatalf("truncate signal tables: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// A single-instance pattern carrying every learned baseline: a global
	// EWMA + variance, a cumulative average, a per-pattern mode override, and
	// two populated hour-of-day seasonal buckets.
	seasonal := make([]stats.EWMA, stats.HoursPerDay)
	seasonal[3] = stats.EWMA{Mean: 44, Variance: 6.25, Count: 100}
	seasonal[14] = stats.EWMA{Mean: 12.5, Variance: 1.5, Count: 37}
	now := time.Now().UTC()
	want := &Pattern{
		ID:                "p-parity",
		OrgID:             storage.DefaultOrgID,
		Template:          "boom <*>",
		Count:             321,
		BaselineFrequency: 3.82,
		BaselineVariance:  63.4,
		BaselineAvg:       11.63,
		SpikeBaselineMode: "average",
		Seasonal:          seasonal,
		FirstSeen:         now,
		LastSeen:          now,
	}

	cs := NewPostgresCatalogStore(db, storage.DefaultOrgID, 0)
	if err := cs.Persist(map[string]*Pattern{want.ID: want}, nil); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	// Load (detail path) and Snapshot (list path) must agree on every baseline.
	loaded, _, err := cs.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	det := loaded[want.ID]
	if det == nil {
		t.Fatalf("pattern %q missing from Load", want.ID)
	}
	snap, _, err := cs.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	lst := patternByID(snap, want.ID)
	if lst == nil {
		t.Fatalf("pattern %q missing from Snapshot", want.ID)
	}

	if lst.BaselineFrequency != det.BaselineFrequency {
		t.Fatalf("baseline_frequency: snapshot %v != load %v", lst.BaselineFrequency, det.BaselineFrequency)
	}
	if lst.BaselineVariance != det.BaselineVariance {
		t.Fatalf("baseline_variance: snapshot %v != load %v", lst.BaselineVariance, det.BaselineVariance)
	}
	if lst.BaselineAvg != det.BaselineAvg {
		t.Fatalf("baseline_avg: snapshot %v != load %v", lst.BaselineAvg, det.BaselineAvg)
	}
	if lst.SpikeBaselineMode != det.SpikeBaselineMode {
		t.Fatalf("spike_baseline_mode: snapshot %q != load %q", lst.SpikeBaselineMode, det.SpikeBaselineMode)
	}
	if len(lst.Seasonal) != len(det.Seasonal) {
		t.Fatalf("seasonal len: snapshot %d != load %d", len(lst.Seasonal), len(det.Seasonal))
	}
	for i := range det.Seasonal {
		if lst.Seasonal[i] != det.Seasonal[i] {
			t.Fatalf("seasonal[%d]: snapshot %+v != load %+v", i, lst.Seasonal[i], det.Seasonal[i])
		}
	}

	// And each round-trips to the value written (Default is populated, not 0).
	if det.BaselineFrequency != want.BaselineFrequency {
		t.Fatalf("baseline_frequency load = %v, want %v", det.BaselineFrequency, want.BaselineFrequency)
	}
	if det.BaselineAvg != want.BaselineAvg {
		t.Fatalf("baseline_avg load = %v, want %v", det.BaselineAvg, want.BaselineAvg)
	}
}
