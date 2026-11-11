package settings

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// Storage keys. Every key is qualified by the record's organization so one
// organization's account, draft or setting can never be read through another
// organization's prefix. The shapes are the connector's public persistence
// contract.
const (
	accountSegment = "accounts/"
	draftSegment   = "drafts/"
	healthSegment  = "health/"
	selfTestName   = "privacy-selftest"
	privacyName    = "privacy"
	optionsName    = "options"
	revisionName   = "accounts-revision"
)

// DraftTTL is how long an unfinished wizard draft survives.
const DraftTTL = time.Hour

var (
	// ErrNotFound means the record does not exist or its draft has expired.
	ErrNotFound = errors.New("aws settings record not found")
	// ErrOutOfScope means the caller's organization is not inside the
	// deployment's tenancy scope, so the record must not be read or written.
	ErrOutOfScope = errors.New("aws settings record outside tenancy scope")
	// ErrNoStorage means the connector has no persistence backend.
	ErrNoStorage = errors.New("aws settings storage unavailable")
	// ErrInvalidRecord means an identifier or stored payload is malformed.
	ErrInvalidRecord = errors.New("invalid aws settings record")
	// ErrUnreadableAccounts means at least one stored account record could
	// not be decoded, so the account list is not complete.
	ErrUnreadableAccounts = errors.New("unreadable_account_records")
	// ErrRevisionContended means the account settings revision kept moving
	// under concurrent writers and could not be raised. The account change
	// itself is stored; options keep reading the older revision, so a
	// removed account stays out of collection, until the next change raises
	// it.
	ErrRevisionContended = errors.New("aws settings revision contended")
)

var (
	accountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)
	draftIDPattern   = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

// ValidAccountID reports whether value is a 12-digit AWS account ID.
func ValidAccountID(value string) bool { return accountIDPattern.MatchString(value) }

// ValidDraftID reports whether value is a server-generated draft identifier.
func ValidDraftID(value string) bool { return draftIDPattern.MatchString(value) }

// orgPrefix is the storage namespace for one organization.
func orgPrefix(org string) string {
	return "aws/" + tenancy.NormalizeOrgID(org) + "/"
}

// AccountPrefix is the storage prefix for one organization's accounts.
func AccountPrefix(org string) string { return orgPrefix(org) + accountSegment }

// DraftPrefix is the storage prefix for one organization's drafts.
func DraftPrefix(org string) string { return orgPrefix(org) + draftSegment }

// HealthPrefix is the storage prefix for one organization's health records.
func HealthPrefix(org string) string { return orgPrefix(org) + healthSegment }

// SelfTestKey is the storage key for one organization's last self-test.
func SelfTestKey(org string) string { return orgPrefix(org) + selfTestName }

// PrivacyKey is the storage key for one organization's privacy settings.
func PrivacyKey(org string) string { return orgPrefix(org) + privacyName }

// OptionsKey is the storage key for one organization's saved options.
func OptionsKey(org string) string { return orgPrefix(org) + optionsName }

// AccountsRevisionKey is the storage key for one organization's account
// settings revision. It sits beside the account records, not under their
// prefix, so listing accounts never reads it.
func AccountsRevisionKey(org string) string { return orgPrefix(org) + revisionName }

// AccountKey is the storage key for one account.
func AccountKey(org, accountID string) string { return AccountPrefix(org) + accountID }

// DraftKey is the storage key for one wizard draft.
func DraftKey(org, draftID string) string { return DraftPrefix(org) + draftID }

// HealthKey is the storage key for one account's health record.
func HealthKey(org, accountID string) string { return HealthPrefix(org) + accountID }

// Store persists connector state through the shared storage provider. It owns
// the tenancy scope: reads are allowed for any organization in scope.Read and
// writes only for scope.Write.
type Store struct {
	provider storage.Provider
	scope    tenancy.OrgScope
	now      func() time.Time

	// accountsMu serializes account writes with the revision they raise,
	// so each change gets its own revision and a removal holds the revision
	// it read until its record is gone.
	accountsMu sync.Mutex
}

// NewStore builds a store over provider. A nil provider is allowed so the API
// can report storage unavailability instead of panicking at boot.
func NewStore(provider storage.Provider, scope tenancy.OrgScope) *Store {
	return &Store{provider: provider, scope: scope.Normalized(), now: time.Now}
}

// setClock replaces the clock in tests.
func (store *Store) setClock(now func() time.Time) {
	if now != nil {
		store.now = now
	}
}

// Scope reports the deployment scope this store is pinned to.
func (store *Store) Scope() tenancy.OrgScope {
	if store == nil {
		return tenancy.DefaultOrgScope()
	}
	return store.scope.Normalized()
}

func (store *Store) canRead(org string) bool {
	return store != nil && store.scope.Contains(org)
}

func (store *Store) canWrite(org string) bool {
	return store != nil && store.scope.Normalized().Write == tenancy.NormalizeOrgID(org)
}

func (store *Store) read(org, key string) ([]byte, error) {
	if store == nil || store.provider == nil {
		return nil, ErrNoStorage
	}
	if !store.canRead(org) {
		return nil, ErrOutOfScope
	}
	return store.provider.ReadBlob(key)
}

func (store *Store) write(org, key string, value any) error {
	if store == nil || store.provider == nil {
		return ErrNoStorage
	}
	if !store.canWrite(org) {
		return ErrOutOfScope
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ErrInvalidRecord
	}
	return store.provider.WriteBlob(key, data)
}

// delete removes a record. A backend that cannot remove the key (the file
// store answers "unsupported" for everything outside model state) gets an
// empty, non-null tombstone instead, which every backend accepts and every
// reader treats as absent. Unsupported is never reported as a successful
// delete.
func (store *Store) delete(org, key string) error {
	if store == nil || store.provider == nil {
		return ErrNoStorage
	}
	if !store.canWrite(org) {
		return ErrOutOfScope
	}
	err := storage.DeleteBlob(store.provider, key)
	if err == nil || errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if !errors.Is(err, storage.ErrUnsupported) {
		return err
	}
	return store.provider.WriteBlob(key, []byte{})
}

// ListAccounts returns every stored wizard account, sorted by account ID.
func (store *Store) ListAccounts(org string) ([]AccountRecord, error) {
	accounts, _, err := store.listAccounts(org)
	return accounts, err
}

// ListAccountsStrict is ListAccounts that fails with ErrUnreadableAccounts
// when a stored record cannot be decoded, for callers that act on absence:
// an account whose record is unreadable is not a removed account.
func (store *Store) ListAccountsStrict(org string) ([]AccountRecord, error) {
	accounts, unreadable, err := store.listAccounts(org)
	if err != nil {
		return nil, err
	}
	if unreadable > 0 {
		return nil, ErrUnreadableAccounts
	}
	return accounts, nil
}

// listAccounts decodes every stored account and counts the records it could
// not read. An empty blob is a deleted record, not an unreadable one.
func (store *Store) listAccounts(org string) ([]AccountRecord, int, error) {
	if store == nil || store.provider == nil {
		return nil, 0, ErrNoStorage
	}
	if !store.canRead(org) {
		return nil, 0, ErrOutOfScope
	}
	blobs, err := store.provider.ListBlobs(AccountPrefix(org))
	if err != nil {
		return nil, 0, err
	}
	accounts := make([]AccountRecord, 0, len(blobs))
	unreadable := 0
	for _, blob := range blobs {
		if len(blob.Data) == 0 {
			continue
		}
		var record AccountRecord
		if json.Unmarshal(blob.Data, &record) != nil || !ValidAccountID(record.AccountID) {
			unreadable++
			continue
		}
		record.Source = SourceWizard
		accounts = append(accounts, record)
	}
	sortAccounts(accounts)
	return accounts, unreadable, nil
}

// GetAccount returns one stored wizard account.
func (store *Store) GetAccount(org, accountID string) (AccountRecord, error) {
	if !ValidAccountID(accountID) {
		return AccountRecord{}, ErrInvalidRecord
	}
	data, err := store.read(org, AccountKey(org, accountID))
	if err != nil {
		return AccountRecord{}, err
	}
	if len(data) == 0 {
		return AccountRecord{}, ErrNotFound
	}
	var record AccountRecord
	if json.Unmarshal(data, &record) != nil || record.AccountID != accountID {
		return AccountRecord{}, ErrInvalidRecord
	}
	record.Source = SourceWizard
	return record, nil
}

// SaveAccount writes one wizard account and raises the account settings
// revision after it.
func (store *Store) SaveAccount(org string, record AccountRecord) error {
	if !ValidAccountID(record.AccountID) {
		return ErrInvalidRecord
	}
	if store == nil {
		return ErrNoStorage
	}
	record.Source = SourceWizard
	if record.CreatedAt.IsZero() {
		record.CreatedAt = store.now().UTC()
	}
	store.accountsMu.Lock()
	defer store.accountsMu.Unlock()
	if err := store.write(org, AccountKey(org, record.AccountID), record); err != nil {
		return err
	}
	_, err := store.raiseRevision(org)
	return err
}

// DeleteAccount removes one wizard account and its health record.
func (store *Store) DeleteAccount(org, accountID string) error {
	return store.RemoveAccount(org, accountID, nil)
}

// RemoveAccount removes one wizard account and its health record, then
// raises the account settings revision. purge, when set, runs first with
// the revision read before the removal; no other account write in this
// process can move the revision until the record is gone, so options read at
// a newer revision were read after the removal. A purge error leaves the
// record in place.
//
// Another replica sharing the storage can still raise the revision while the
// record is being removed, and options read at that revision may list the
// account. When that happened, purge runs again with the revision just below
// the one this removal raised, so only options read after the record was
// gone can collect the account again.
func (store *Store) RemoveAccount(org, accountID string, purge func(revision uint64) error) error {
	if !ValidAccountID(accountID) {
		return ErrInvalidRecord
	}
	if store == nil {
		return ErrNoStorage
	}
	store.accountsMu.Lock()
	defer store.accountsMu.Unlock()
	var before uint64
	if purge != nil {
		revision, err := store.AccountsRevision(org)
		if err != nil {
			return err
		}
		if err := purge(revision); err != nil {
			return err
		}
		before = revision
	}
	if err := store.delete(org, AccountKey(org, accountID)); err != nil {
		return err
	}
	if err := store.delete(org, HealthKey(org, accountID)); err != nil {
		return err
	}
	after, err := store.raiseRevision(org)
	if err != nil {
		return err
	}
	if purge != nil && after-1 > before {
		return purge(after - 1)
	}
	return nil
}

// accountsRevision is the stored account settings revision.
type accountsRevision struct {
	Revision uint64 `json:"revision"`
}

// AccountsRevision returns the account settings revision: a counter raised
// after every account save or removal and kept with the account records, so
// it only grows across restarts and is independent of any clock. Read it
// before listing the accounts it describes. A missing record is zero.
func (store *Store) AccountsRevision(org string) (uint64, error) {
	data, err := store.read(org, AccountsRevisionKey(org))
	if err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, nil
	}
	var record accountsRevision
	if json.Unmarshal(data, &record) != nil {
		return 0, ErrInvalidRecord
	}
	return record.Revision, nil
}

// revisionRaiseAttempts bounds the compare-and-swap retries of one raise.
// Each lost attempt means another writer raised the revision, so running
// out takes that many concurrent account changes.
const revisionRaiseAttempts = 64

// raiseRevision adds one to the account settings revision and returns the
// new value. The caller holds accountsMu, which orders the raises of this
// process. Replicas sharing the storage each hold their own lock, so on a
// backend with compare-and-swap the raise only succeeds against the value it
// read, and a lost race reads again: two raisers never store or return the
// same revision. A backend without it serves a single process, where the
// lock is enough.
func (store *Store) raiseRevision(org string) (uint64, error) {
	if store == nil || store.provider == nil {
		return 0, ErrNoStorage
	}
	if !store.canWrite(org) {
		return 0, ErrOutOfScope
	}
	cas, ok := store.provider.(storage.BlobCAS)
	if !ok {
		current, err := store.AccountsRevision(org)
		if err != nil {
			return 0, err
		}
		next := current + 1
		return next, store.write(org, AccountsRevisionKey(org), accountsRevision{Revision: next})
	}
	key := AccountsRevisionKey(org)
	for range revisionRaiseAttempts {
		stored, err := store.provider.ReadBlob(key)
		if err != nil {
			return 0, err
		}
		var current accountsRevision
		if len(stored) > 0 && json.Unmarshal(stored, &current) != nil {
			return 0, ErrInvalidRecord
		}
		next := current.Revision + 1
		data, err := json.Marshal(accountsRevision{Revision: next})
		if err != nil {
			return 0, ErrInvalidRecord
		}
		swapped, err := cas.CompareAndSwapBlob(key, stored, data)
		if err != nil {
			return 0, err
		}
		if swapped {
			return next, nil
		}
	}
	return 0, ErrRevisionContended
}

// GetDraft returns a live draft. An expired draft answers ErrNotFound and is
// removed on the way out.
func (store *Store) GetDraft(org, draftID string) (Draft, error) {
	if !ValidDraftID(draftID) {
		return Draft{}, ErrInvalidRecord
	}
	data, err := store.read(org, DraftKey(org, draftID))
	if err != nil {
		return Draft{}, err
	}
	if len(data) == 0 {
		return Draft{}, ErrNotFound
	}
	var draft Draft
	if json.Unmarshal(data, &draft) != nil || draft.ID != draftID {
		return Draft{}, ErrInvalidRecord
	}
	if !draft.ExpiresAt.IsZero() && !store.now().Before(draft.ExpiresAt) {
		_ = store.delete(org, DraftKey(org, draftID))
		return Draft{}, ErrNotFound
	}
	return draft, nil
}

// SaveDraft writes a draft and stamps its expiry on first save.
func (store *Store) SaveDraft(org string, draft Draft) error {
	if !ValidDraftID(draft.ID) {
		return ErrInvalidRecord
	}
	if draft.CreatedAt.IsZero() {
		draft.CreatedAt = store.now().UTC()
	}
	if draft.ExpiresAt.IsZero() {
		draft.ExpiresAt = draft.CreatedAt.Add(DraftTTL)
	}
	return store.write(org, DraftKey(org, draft.ID), draft)
}

// DeleteDraft removes one draft.
func (store *Store) DeleteDraft(org, draftID string) error {
	if !ValidDraftID(draftID) {
		return ErrInvalidRecord
	}
	return store.delete(org, DraftKey(org, draftID))
}

// GetHealth returns one health record. A missing record is not an error; it
// answers the zero record so the caller can render "never checked".
func (store *Store) GetHealth(org, accountID string) (HealthRecord, error) {
	if !ValidAccountID(accountID) {
		return HealthRecord{}, ErrInvalidRecord
	}
	data, err := store.read(org, HealthKey(org, accountID))
	if err != nil {
		return HealthRecord{}, err
	}
	if len(data) == 0 {
		return HealthRecord{AccountID: accountID, Status: StatusUnknown}, nil
	}
	var record HealthRecord
	if json.Unmarshal(data, &record) != nil {
		return HealthRecord{}, ErrInvalidRecord
	}
	record.AccountID = accountID
	if record.Status == "" {
		record.Status = StatusUnknown
	}
	return record, nil
}

// SaveHealth writes one health record.
func (store *Store) SaveHealth(org string, record HealthRecord) error {
	if !ValidAccountID(record.AccountID) {
		return ErrInvalidRecord
	}
	return store.write(org, HealthKey(org, record.AccountID), record)
}

// DeleteHealth removes one health record.
func (store *Store) DeleteHealth(org, accountID string) error {
	if !ValidAccountID(accountID) {
		return ErrInvalidRecord
	}
	return store.delete(org, HealthKey(org, accountID))
}

// LoadSelfTest returns the last privacy self-test. A missing record is the
// zero value, which the settings API renders as "not run yet".
func (store *Store) LoadSelfTest(org string) (SelfTestRecord, error) {
	data, err := store.read(org, SelfTestKey(org))
	if err != nil {
		return SelfTestRecord{}, err
	}
	if len(data) == 0 {
		return SelfTestRecord{}, nil
	}
	var record SelfTestRecord
	if json.Unmarshal(data, &record) != nil {
		return SelfTestRecord{}, ErrInvalidRecord
	}
	return record, nil
}

// SaveSelfTest writes the privacy self-test record.
func (store *Store) SaveSelfTest(org string, record SelfTestRecord) error {
	return store.write(org, SelfTestKey(org), record)
}

// LoadPrivacy returns the persisted tag-value allowlist and leak samples.
func (store *Store) LoadPrivacy(org string) (privacyRecord, error) {
	data, err := store.read(org, PrivacyKey(org))
	if err != nil {
		return privacyRecord{}, err
	}
	if len(data) == 0 {
		return privacyRecord{}, nil
	}
	var record privacyRecord
	if json.Unmarshal(data, &record) != nil {
		return privacyRecord{}, ErrInvalidRecord
	}
	return record, nil
}

// SavePrivacy writes the tag-value allowlist and leak samples.
func (store *Store) SavePrivacy(org string, record privacyRecord) error {
	return store.write(org, PrivacyKey(org), record)
}

// LoadOptions returns the stored wizard overrides. A missing record is empty,
// meaning the config values apply.
func (store *Store) LoadOptions(org string) (Options, error) {
	data, err := store.read(org, OptionsKey(org))
	if err != nil {
		return Options{}, err
	}
	if len(data) == 0 {
		return Options{}, nil
	}
	var options Options
	if json.Unmarshal(data, &options) != nil {
		return Options{}, ErrInvalidRecord
	}
	return options, nil
}

// SaveOptions writes the wizard overrides.
func (store *Store) SaveOptions(org string, options Options) error {
	return store.write(org, OptionsKey(org), options)
}

// SanitizeAllowlist trims, de-duplicates and bounds the tag keys whose
// values may reach the model. It returns a stable order. AWS tag keys are
// case-sensitive, so a key keeps its case and is matched exactly: "Name"
// and "name" are two keys.
func SanitizeAllowlist(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	clean := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 63 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		clean = append(clean, value)
		if len(clean) == 32 {
			break
		}
	}
	return clean
}

// DefaultTagValueAllowlist is the least-privilege starting set.
func DefaultTagValueAllowlist() []string {
	return []string{"app", "team", "env", "owner", "cost-center"}
}

func sortAccounts(accounts []AccountRecord) {
	for index := 1; index < len(accounts); index++ {
		for current := index; current > 0 && accounts[current].AccountID < accounts[current-1].AccountID; current-- {
			accounts[current], accounts[current-1] = accounts[current-1], accounts[current]
		}
	}
}
