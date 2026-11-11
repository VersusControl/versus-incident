package settings

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/probe"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(storage.NewMemory(), tenancy.DefaultOrgScope())
}

func TestExternalIDShapeAndMask(t *testing.T) {
	first, err := GenerateExternalID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateExternalID()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("external IDs repeated")
	}
	if !strings.HasPrefix(first, ExternalIDPrefix) {
		t.Fatalf("external id %q has no prefix", first)
	}
	body := strings.TrimPrefix(first, ExternalIDPrefix)
	if len(body) != 24 {
		t.Fatalf("body length = %d, want 24", len(body))
	}
	for _, character := range body {
		if (character < 'a' || character > 'z') && (character < '2' || character > '7') {
			t.Fatalf("external id body has non-base32 character %q", character)
		}
	}
	masked := MaskExternalID(first)
	if strings.Contains(masked, body) {
		t.Fatalf("mask %q reveals the full body", masked)
	}
	if !strings.HasSuffix(masked, body[len(body)-4:]) {
		t.Fatalf("mask %q hides the distinguishing tail", masked)
	}
}

func TestStoreUsesExactKeysAndEnforcesScope(t *testing.T) {
	provider := storage.NewMemory()
	store := NewStore(provider, tenancy.NewOrgScope("licensed", "default"))
	if err := store.SaveAccount("licensed", AccountRecord{AccountID: "444455556666", DisplayName: "Staging"}); err != nil {
		t.Fatal(err)
	}
	data, err := provider.ReadBlob(AccountKey("licensed", "444455556666"))
	if err != nil || len(data) == 0 {
		t.Fatalf("record not stored at the exact key: %v", err)
	}
	if err := store.SaveAccount("default", AccountRecord{AccountID: "111122223333"}); err != ErrOutOfScope {
		t.Fatalf("write outside scope = %v, want ErrOutOfScope", err)
	}
	if _, err := store.ListAccounts("default"); err != nil {
		t.Fatalf("read from a scoped organization failed: %v", err)
	}
	if _, err := store.ListAccounts("other-org"); err != ErrOutOfScope {
		t.Fatalf("read outside scope = %v, want ErrOutOfScope", err)
	}
}

func TestDraftExpiresAfterOneHour(t *testing.T) {
	store := testStore(t)
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	store.setClock(func() time.Time { return now })
	draft := Draft{ID: strings.Repeat("a", 32), ExternalID: "vrs-ext-abcdefghijklmnopqrstuvwx", Status: DraftStarted}
	if err := store.SaveDraft("default", draft); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetDraft("default", draft.ID); err != nil {
		t.Fatalf("live draft unreadable: %v", err)
	}
	now = now.Add(DraftTTL + time.Second)
	if _, err := store.GetDraft("default", draft.ID); err != ErrNotFound {
		t.Fatalf("expired draft = %v, want ErrNotFound", err)
	}
}

func TestMergeAccountsConfigWinsOnSameID(t *testing.T) {
	stored := []AccountRecord{
		{AccountID: "111122223333", DisplayName: "Wizard production", Source: SourceWizard},
		{AccountID: "444455556666", DisplayName: "Staging", Source: SourceWizard},
	}
	configured := []config.AWSAccountConfig{{
		AccountID:   "111122223333",
		DisplayName: "Production",
		Auth:        config.AWSAuthConfig{RoleARN: "arn:aws:iam::111122223333:role/versus-reader"},
	}}
	merged := MergeAccounts(stored, configured)
	if len(merged) != 2 {
		t.Fatalf("merged = %d accounts, want 2", len(merged))
	}
	if merged[0].DisplayName != "Production" || merged[0].Source != SourceConfig {
		t.Fatalf("config did not win: %+v", merged[0])
	}
	if merged[1].DisplayName != "Staging" || merged[1].Source != SourceWizard {
		t.Fatalf("wizard account lost: %+v", merged[1])
	}
}

func TestDraftViewNeverCarriesTheFullExternalID(t *testing.T) {
	externalID, err := GenerateExternalID()
	if err != nil {
		t.Fatal(err)
	}
	draft := Draft{ID: strings.Repeat("b", 32), ExternalID: externalID, Status: DraftVerified}
	encoded, err := json.Marshal(draft.View())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), externalID) {
		t.Fatal("draft view contains the full external id")
	}
	if !strings.Contains(string(encoded), MaskExternalID(externalID)) {
		t.Fatal("draft view is missing the masked external id")
	}
}

func TestCountResultsKeepsMissingServicesNotChecked(t *testing.T) {
	counts := CountResults([]probe.Result{
		{Service: "ec2", Status: probe.StatusReadable, Items: 3},
		{Service: "elasticache", Status: probe.StatusDenied},
		{Service: "aws-health", Status: probe.StatusNotOnPlan},
	})
	if counts.Total != 24 {
		t.Fatalf("total = %d", counts.Total)
	}
	if counts.Readable != 1 || len(counts.Denied) != 1 || len(counts.NotOnPlan) != 1 {
		t.Fatalf("counts = %+v", counts)
	}
	if counts.NotChecked != counts.Total-3 || len(counts.NotCheckedServices) != counts.NotChecked {
		t.Fatalf("not checked = %d, want %d", counts.NotChecked, counts.Total-3)
	}
	for _, service := range counts.NotCheckedServices {
		if service == "ec2" || service == "elasticache" || service == "aws-health" {
			t.Fatalf("answered service %q reported as not checked", service)
		}
	}
}

func TestSanitizeAllowlistBoundsAndDeduplicates(t *testing.T) {
	clean := SanitizeAllowlist([]string{" app ", "app", "", "team", strings.Repeat("x", 64)})
	if len(clean) != 2 || clean[0] != "app" || clean[1] != "team" {
		t.Fatalf("clean = %v", clean)
	}
}

// AWS tag keys are case-sensitive: the allowlist keeps each key's case, so
// "Name" can be allowlisted and "name" is a different key.
func TestSanitizeAllowlistKeepsTheKeyCase(t *testing.T) {
	clean := SanitizeAllowlist([]string{" Name ", "name", "Name", "Team"})
	if len(clean) != 3 || clean[0] != "Name" || clean[1] != "name" || clean[2] != "Team" {
		t.Fatalf("clean = %v", clean)
	}
}

func TestStoreScopesEveryKeyToItsOrg(t *testing.T) {
	provider := storage.NewMemory()
	store := NewStore(provider, tenancy.NewOrgScope("licensed", "default"))
	accountID := "444455556666"
	draftID := strings.Repeat("d", 32)
	if err := store.SaveAccount("licensed", AccountRecord{AccountID: accountID, DisplayName: "Staging"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveHealth("licensed", HealthRecord{AccountID: accountID, Status: StatusConnected}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDraft("licensed", Draft{ID: draftID, ExternalID: "vrs-ext-abcdefghijklmnopqrstuvwx"}); err != nil {
		t.Fatal(err)
	}
	budget := 9
	if err := store.SaveOptions("licensed", Options{MaxRequestsPerSecond: &budget}); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePrivacy("licensed", privacyRecord{TagValueAllowlist: []string{"team"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSelfTest("licensed", SelfTestRecord{Passing: true}); err != nil {
		t.Fatal(err)
	}

	// The owning org reads everything back.
	if _, err := store.GetAccount("licensed", accountID); err != nil {
		t.Fatalf("owner account unreadable: %v", err)
	}
	if _, err := store.GetDraft("licensed", draftID); err != nil {
		t.Fatalf("owner draft unreadable: %v", err)
	}

	// Another in-scope org sees none of it through the same provider.
	if _, err := store.GetAccount("default", accountID); err != ErrNotFound {
		t.Fatalf("read-org account = %v, want ErrNotFound", err)
	}
	accounts, err := store.ListAccounts("default")
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 0 {
		t.Fatalf("read-org listed another org's accounts: %+v", accounts)
	}
	if _, err := store.GetDraft("default", draftID); err != ErrNotFound {
		t.Fatalf("read-org draft = %v, want ErrNotFound", err)
	}
	health, err := store.GetHealth("default", accountID)
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != StatusUnknown || !health.CheckedAt.IsZero() {
		t.Fatalf("read-org health = %+v, want the empty record", health)
	}
	options, err := store.LoadOptions("default")
	if err != nil {
		t.Fatal(err)
	}
	if options.MaxRequestsPerSecond != nil {
		t.Fatalf("read-org options = %+v", options)
	}
	privacy, err := store.LoadPrivacy("default")
	if err != nil {
		t.Fatal(err)
	}
	if len(privacy.TagValueAllowlist) != 0 {
		t.Fatalf("read-org privacy = %+v", privacy)
	}
	selfTest, err := store.LoadSelfTest("default")
	if err != nil {
		t.Fatal(err)
	}
	if !selfTest.LastRunAt.IsZero() || selfTest.Passing {
		t.Fatalf("read-org self-test = %+v", selfTest)
	}

	// A read org cannot write into the owner's namespace either.
	if err := store.SaveAccount("default", AccountRecord{AccountID: "111122223333"}); err != ErrOutOfScope {
		t.Fatalf("read-org write = %v, want ErrOutOfScope", err)
	}
}

// TestAccountsRevisionGrowsWithEveryAccountChange: every save and removal
// raises the revision after the record is written; a fresh store over the
// same storage reads the same revision, as a restarted process does; the
// revision record is not under the account prefix, so a listing never reads
// it; and a removal hands its purge the revision read before the record goes.
func TestAccountsRevisionGrowsWithEveryAccountChange(t *testing.T) {
	provider := storage.NewMemory()
	store := NewStore(provider, tenancy.DefaultOrgScope())
	const org = "default"
	revision := func(s *Store) uint64 {
		t.Helper()
		value, err := s.AccountsRevision(org)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if got := revision(store); got != 0 {
		t.Fatalf("a fresh deployment's revision = %d", got)
	}
	for _, id := range []string{"111122223333", "444455556666"} {
		if err := store.SaveAccount(org, AccountRecord{AccountID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if got := revision(store); got != 2 {
		t.Fatalf("after two saves the revision = %d", got)
	}
	if accounts, err := store.ListAccountsStrict(org); err != nil || len(accounts) != 2 {
		t.Fatalf("the revision record is read as an account: %+v %v", accounts, err)
	}

	var seen uint64
	err := store.RemoveAccount(org, "111122223333", func(r uint64) error {
		seen = r
		if _, err := store.GetAccount(org, "111122223333"); err != nil {
			t.Fatalf("the record is gone before the purge: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 2 || revision(store) != 3 {
		t.Fatalf("purge saw %d, revision after removal %d", seen, revision(store))
	}
	if got := revision(NewStore(provider, tenancy.DefaultOrgScope())); got != 3 {
		t.Fatalf("a restarted store reads revision %d", got)
	}

	failed := errors.New("purge failed")
	if err := store.RemoveAccount(org, "444455556666", func(uint64) error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("purge error = %v", err)
	}
	if _, err := store.GetAccount(org, "444455556666"); err != nil || revision(store) != 3 {
		t.Fatalf("a failed purge removed the record or moved the revision: %v, %d", err, revision(store))
	}
}

// TestRemovalHoldsTheRevisionUntilTheRecordIsGone: a save that arrives while
// a removal is purging waits, so no options can read a newer revision while
// the removed record is still listed.
func TestRemovalHoldsTheRevisionUntilTheRecordIsGone(t *testing.T) {
	store := testStore(t)
	const org = "default"
	if err := store.SaveAccount(org, AccountRecord{AccountID: "111122223333"}); err != nil {
		t.Fatal(err)
	}
	saved := make(chan error, 1)
	err := store.RemoveAccount(org, "111122223333", func(uint64) error {
		go func() { saved <- store.SaveAccount(org, AccountRecord{AccountID: "444455556666"}) }()
		select {
		case err := <-saved:
			t.Fatalf("a save ran during the removal: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		if got, err := store.AccountsRevision(org); err != nil || got != 1 {
			t.Fatalf("the revision moved during the removal: %d %v", got, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	if got, err := store.AccountsRevision(org); err != nil || got != 3 {
		t.Fatalf("revision after the removal and the save = %d %v", got, err)
	}
}

// revisionRemover records the revision each payer removal carries and the
// revision the store held while it ran.
type revisionRemover struct {
	store *Store
	got   []uint64
	held  []uint64
}

func (r *revisionRemover) MarkPayerRemoved(_ context.Context, _ string, revision uint64) error {
	held, err := r.store.AccountsRevision("default")
	if err != nil {
		return err
	}
	r.got, r.held = append(r.got, revision), append(r.held, held)
	return nil
}

func (r *revisionRemover) PurgePayer(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}

// TestDeleteAccountHandsTheCostRemovalItsRevision: deleting an account tells
// the cost adapter the revision read before the removal, while the store
// still holds it; the removal then raises it, so only options read afterwards
// are newer.
func TestDeleteAccountHandsTheCostRemovalItsRevision(t *testing.T) {
	store := testStore(t)
	remover := &revisionRemover{store: store}
	service := NewService(store, WithConfig(collectingConfig(true)), WithPayerRemover(remover))
	for _, id := range []string{"111122223333", "444455556666"} {
		if err := store.SaveAccount("default", AccountRecord{AccountID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.DeleteAccount(context.Background(), "default", "111122223333"); err != nil {
		t.Fatal(err)
	}
	after, err := service.AccountsRevision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(remover.got) != 1 || remover.got[0] != 2 || remover.held[0] != 2 || after != 3 {
		t.Fatalf("removal carried %v while the store held %v; revision after %d", remover.got, remover.held, after)
	}
}

// stallingRemover marks payers removed and holds every purge until the test
// releases it, then reports the purge deferred, standing in for a cost
// refresh that holds the cost lock.
type stallingRemover struct {
	marked  []uint64
	wait    time.Duration
	purges  int
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newStallingRemover(t *testing.T) *stallingRemover {
	r := &stallingRemover{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(r.free)
	return r
}

func (r *stallingRemover) free() { r.once.Do(func() { close(r.release) }) }

func (r *stallingRemover) MarkPayerRemoved(_ context.Context, _ string, revision uint64) error {
	r.marked = append(r.marked, revision)
	return nil
}

func (r *stallingRemover) PurgePayer(_ context.Context, _ string, wait time.Duration) (bool, error) {
	r.purges++
	r.wait = wait
	close(r.entered)
	<-r.release
	return false, nil
}

// TestDeleteAccountHoldsNoAccountLockWhileTheCostPurgeWaits: the cost purge
// of a deleted account waits for a running cost refresh after the account
// record is gone and the tombstone stored, with the account lock released:
// another account write completes meanwhile. The purge is bounded by at
// most five seconds, and the removal answers success with the purge
// pending.
func TestDeleteAccountHoldsNoAccountLockWhileTheCostPurgeWaits(t *testing.T) {
	store := testStore(t)
	remover := newStallingRemover(t)
	service := NewService(store, WithConfig(collectingConfig(true)), WithPayerRemover(remover))
	for _, id := range []string{"111122223333", "444455556666"} {
		if err := store.SaveAccount("default", AccountRecord{AccountID: id}); err != nil {
			t.Fatal(err)
		}
	}
	type outcome struct {
		removal AccountRemoval
		err     error
	}
	removed := make(chan outcome, 1)
	go func() {
		removal, err := service.DeleteAccount(context.Background(), "default", "111122223333")
		removed <- outcome{removal, err}
	}()
	select {
	case <-remover.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the cost purge never started")
	}
	if accounts, err := store.ListAccounts("default"); err != nil || len(accounts) != 1 || accounts[0].AccountID != "444455556666" {
		t.Fatalf("while the purge waits the record must already be gone: %+v %v", accounts, err)
	}
	saved := make(chan error, 1)
	go func() { saved <- store.SaveAccount("default", AccountRecord{AccountID: "777788889999"}) }()
	select {
	case err := <-saved:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an account write waited for the cost purge: the account lock is held while it waits")
	}
	remover.free()
	got := <-removed
	if got.err != nil || !got.removal.PurgePending {
		t.Fatalf("delete = %+v %v, want success with the purge pending", got.removal, got.err)
	}
	if remover.wait <= 0 || remover.wait > 5*time.Second {
		t.Fatalf("the purge may wait %s; the bound is five seconds", remover.wait)
	}
	if len(remover.marked) != 1 || remover.marked[0] != 2 || remover.purges != 1 {
		t.Fatalf("marked at %v with %d purges, want the revision before the removal and one purge", remover.marked, remover.purges)
	}
}

// contendedRevision makes every compare-and-swap of the account settings
// revision lose once contend is set, standing in for writers on other
// replicas that keep raising it.
type contendedRevision struct {
	storage.Provider
	key     string
	contend bool
	mu      sync.Mutex
}

func (p *contendedRevision) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	p.mu.Lock()
	contend := p.contend && name == p.key
	p.mu.Unlock()
	if contend {
		return false, nil
	}
	return p.Provider.(storage.BlobCAS).CompareAndSwapBlob(name, expected, replacement)
}

// TestDeleteAccountWithAContendedRevisionAnswersPurgePending: the account
// record is removed, then the revision raise runs out of attempts. The
// removal is done and its tombstone stored, so the delete succeeds with the
// purge pending instead of reporting a failure.
func TestDeleteAccountWithAContendedRevisionAnswersPurgePending(t *testing.T) {
	provider := &contendedRevision{Provider: storage.NewMemory(), key: AccountsRevisionKey("default")}
	store := NewStore(provider, tenancy.DefaultOrgScope())
	remover := &revisionRemover{store: store}
	service := NewService(store, WithConfig(collectingConfig(true)), WithPayerRemover(remover))
	if err := store.SaveAccount("default", AccountRecord{AccountID: "111122223333"}); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.contend = true
	provider.mu.Unlock()
	removal, err := service.DeleteAccount(context.Background(), "default", "111122223333")
	if err != nil || !removal.PurgePending {
		t.Fatalf("delete = %+v %v, want success with the purge pending", removal, err)
	}
	if accounts, err := store.ListAccounts("default"); err != nil || len(accounts) != 0 {
		t.Fatalf("the record survived: %+v %v", accounts, err)
	}
	if len(remover.got) != 1 || remover.got[0] != 1 {
		t.Fatalf("the tombstone carried %v, want the revision before the removal", remover.got)
	}
	if revision, err := store.AccountsRevision("default"); err != nil || revision != 1 {
		t.Fatalf("revision = %d %v; the contended raise must not have moved it", revision, err)
	}
}

// revisionBackends are the backends replicas share; each has
// compare-and-swap, so two stores over one provider stand for two replicas
// with their own in-process locks.
func revisionBackends() []backend {
	var out []backend
	for _, candidate := range deleteBackends() {
		switch candidate.name {
		case "memory", "file", "postgres":
			out = append(out, candidate)
		}
	}
	return out
}

// revisionTestOrg is an organization unique to one run, so Postgres runs
// sharing a database start from revision zero.
func revisionTestOrg() string {
	return "rev-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// TestConcurrentRevisionRaisesNeverRepeat: two replicas raising the account
// settings revision at once each get their own value, and the stored
// revision counts every raise.
func TestConcurrentRevisionRaisesNeverRepeat(t *testing.T) {
	for _, candidate := range revisionBackends() {
		t.Run(candidate.name, func(t *testing.T) {
			provider := candidate.provider(t)
			org := revisionTestOrg()
			scope := tenancy.NewOrgScope(org)
			replicas := []*Store{NewStore(provider, scope), NewStore(provider, scope)}
			const perReplica = 20
			results := make(chan uint64, len(replicas)*perReplica)
			errs := make(chan error, len(replicas)*perReplica)
			var wait sync.WaitGroup
			for _, replica := range replicas {
				for range perReplica {
					wait.Add(1)
					go func(store *Store) {
						defer wait.Done()
						// Each replica orders its own raises; only the
						// storage orders the two replicas.
						store.accountsMu.Lock()
						defer store.accountsMu.Unlock()
						revision, err := store.raiseRevision(org)
						if err != nil {
							errs <- err
							return
						}
						results <- revision
					}(replica)
				}
			}
			wait.Wait()
			close(results)
			close(errs)
			for err := range errs {
				t.Fatalf("raise failed: %v", err)
			}
			seen := map[uint64]bool{}
			for revision := range results {
				if seen[revision] {
					t.Fatalf("two raisers returned revision %d", revision)
				}
				seen[revision] = true
			}
			total := uint64(len(replicas) * perReplica)
			if uint64(len(seen)) != total {
				t.Fatalf("%d raises returned %d revisions", total, len(seen))
			}
			for revision := uint64(1); revision <= total; revision++ {
				if !seen[revision] {
					t.Fatalf("revision %d was skipped: %v", revision, seen)
				}
			}
			if stored, err := replicas[0].AccountsRevision(org); err != nil || stored != total {
				t.Fatalf("stored revision = %d %v, want %d", stored, err, total)
			}
		})
	}
}

// TestRemovalOnAnotherReplicaPurgesAgainAtTheNewerRevision: while one
// replica purges a removed account, another replica saves an account and
// raises the revision, so options read at that revision may still list the
// removed one. The removal then purges again with the revision just below
// its own, so only options read after the record was gone can collect it.
func TestRemovalOnAnotherReplicaPurgesAgainAtTheNewerRevision(t *testing.T) {
	for _, candidate := range revisionBackends() {
		t.Run(candidate.name, func(t *testing.T) {
			provider := candidate.provider(t)
			org := revisionTestOrg()
			scope := tenancy.NewOrgScope(org)
			removing, saving := NewStore(provider, scope), NewStore(provider, scope)
			if err := removing.SaveAccount(org, AccountRecord{AccountID: "111122223333"}); err != nil {
				t.Fatal(err)
			}
			var purged []uint64
			var listedDuringRemoval bool
			err := removing.RemoveAccount(org, "111122223333", func(revision uint64) error {
				purged = append(purged, revision)
				if len(purged) > 1 {
					return nil
				}
				if err := saving.SaveAccount(org, AccountRecord{AccountID: "444455556666"}); err != nil {
					return err
				}
				accounts, err := saving.ListAccountsStrict(org)
				if err != nil {
					return err
				}
				for _, account := range accounts {
					listedDuringRemoval = listedDuringRemoval || account.AccountID == "111122223333"
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			after, err := removing.AccountsRevision(org)
			if err != nil {
				t.Fatal(err)
			}
			if !listedDuringRemoval {
				t.Fatal("the other replica's options did not list the account being removed")
			}
			if after != 3 || len(purged) != 2 || purged[0] != 1 || purged[1] != after-1 {
				t.Fatalf("purges at %v with the revision now %d; want 1 then %d", purged, after, after-1)
			}

			single := NewStore(provider, tenancy.NewOrgScope(org))
			var alone []uint64
			if err := single.RemoveAccount(org, "444455556666", func(revision uint64) error {
				alone = append(alone, revision)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(alone) != 1 || alone[0] != after {
				t.Fatalf("an uncontended removal purged at %v, want once at %d", alone, after)
			}
		})
	}
}
