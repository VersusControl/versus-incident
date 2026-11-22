package settings

import (
	"context"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
)

// Account sync states shown on the settings row.
const (
	// SyncStateOff means collection is turned off for the deployment.
	SyncStateOff = "off"
	// SyncStatePending means no unit of the account has been attempted yet.
	SyncStatePending = "pending"
	// SyncStateSyncing means the first sync is still filling in.
	SyncStateSyncing = "syncing"
	// SyncStateOK means every unit's last attempt committed.
	SyncStateOK = "ok"
	// SyncStatePartial means some units failed and serve their last good
	// snapshot.
	SyncStatePartial = "partial"
	// SyncStateFailed means every attempted unit failed.
	SyncStateFailed = "failed"
)

// AccountSyncView is the per-account "Synced N s ago" line and the
// first-sync progress per service. SyncedAt is the newest committed unit;
// the UI renders the age.
type AccountSyncView struct {
	State     string            `json:"state"`
	SyncedAt  *time.Time        `json:"synced_at"`
	FirstSync bool              `json:"first_sync"`
	UnitsDone int               `json:"units_done"`
	Units     int               `json:"units_total"`
	Services  []ServiceSyncView `json:"services"`
}

// ServiceSyncView is one service across the account's regions.
type ServiceSyncView struct {
	Service    string     `json:"service"`
	State      string     `json:"state"`
	UnitsDone  int        `json:"units_done"`
	Units      int        `json:"units_total"`
	LastGoodAt *time.Time `json:"last_good_at"`
	Gaps       []string   `json:"gaps"`
}

// CollectTarget is one account the collectors may read: its regions and the
// credentials of its assumed reader role.
type CollectTarget struct {
	AccountID   string
	Regions     []string
	Credentials aws.CredentialsProvider
	// Cause is set only on a cost target that cannot be read now (see
	// CostTargets); Credentials are then nil and Since says from when.
	Cause string
	Since time.Time
}

// Closed causes for a cost target that cannot be read now. They name what
// the FinOps page shows beside the stored cost it keeps serving.
const (
	// CauseCredentialsInvalid: the server's AWS credentials worked before
	// and are now rejected (expired or revoked).
	CauseCredentialsInvalid = "credentials_invalid"
	// CauseNoServerIdentity: the server has no AWS identity at all.
	CauseNoServerIdentity = "no_server_identity"
	// CauseAccountMismatch: the server's credentials belong to another AWS
	// account than the one configured.
	CauseAccountMismatch = "account_mismatch"
	// CauseAccountDisconnected: any other loss of access to the account.
	CauseAccountDisconnected = "account_disconnected"
)

// WithSnapshots lets the settings view read collection progress from the
// committed snapshot store.
func WithSnapshots(store *snapshot.Store) Option {
	return func(conn *Service) { conn.snapshots = store }
}

// PayerRemover deletes the cost data of an account that stops being a cost
// source. The cost adapter implements it; settings depends only on this.
type PayerRemover interface {
	// MarkPayerRemoved stores that payer stopped being a cost source.
	// revision is the account settings revision read before the account
	// was removed: options read at that revision or earlier may still name
	// it, so it stays out of collection. It never waits for a cost refresh.
	MarkPayerRemoved(ctx context.Context, payer string, revision uint64) error
	// PurgePayer deletes a payer already marked removed. It waits at most
	// wait for a cost refresh to finish, holding no lock meanwhile, and
	// reports purged=false when it did not get to run; a cost refresh
	// purges the payer then.
	PurgePayer(ctx context.Context, payer string, wait time.Duration) (purged bool, err error)
}

// WithPayerRemover removes a deleted account's cost data.
func WithPayerRemover(remover PayerRemover) Option {
	return func(conn *Service) { conn.SetPayerRemover(remover) }
}

// SetPayerRemover sets the cost data remover after the service is built,
// for boot code that builds the cost adapter later.
func (conn *Service) SetPayerRemover(remover PayerRemover) {
	conn.payersMu.Lock()
	defer conn.payersMu.Unlock()
	conn.payers = remover
}

func (conn *Service) payerRemover() PayerRemover {
	conn.payersMu.RLock()
	defer conn.payersMu.RUnlock()
	return conn.payers
}

// WithAccountAdded runs hook after an account is connected, so the first
// sync can start at once.
func WithAccountAdded(hook func(accountID string)) Option {
	return func(conn *Service) { conn.accountAdded = hook }
}

// Limiter is the shared limiter the probes use. The collectors must be
// handed the same one so both draw on one budget per account and region.
func (conn *Service) Limiter() *limit.Manager { return conn.limiter }

// TagValueAllowlist is the effective tag-value allowlist: config first, then
// the saved wizard value, then the least-privilege default.
func (conn *Service) TagValueAllowlist() []string {
	allowlist, _ := conn.allowlistWithSource(conn.storageOrg())
	return allowlist
}

// AccountsRevision is the account settings revision. Read it before
// CollectTargets: a payer removed at a revision is collectable again only
// under targets read at a newer one.
func (conn *Service) AccountsRevision(context.Context) (uint64, error) {
	return conn.store.AccountsRevision(conn.storageOrg())
}

// CollectTargets lists every account the collectors should read now. An
// account without a usable reader role or without regions is skipped; its
// units keep their last good snapshot and the settings row says why.
func (conn *Service) CollectTargets(ctx context.Context) []CollectTarget {
	return conn.targets(ctx, true)
}

// CostTargets lists every account cost is read through. Cost data is global
// (one endpoint, no region list), so unlike CollectTargets an account whose
// regions are still unknown — "auto" before its first health check — is
// included, signed for the home region. An account that cannot be read now
// (disconnected, or its reader role cannot be assumed) is listed with a
// Cause and no credentials: nothing is fetched for it, but the cost already
// stored for it stays on the page, marked stale.
func (conn *Service) CostTargets(ctx context.Context) []CollectTarget {
	return conn.targets(ctx, false)
}

func (conn *Service) targets(ctx context.Context, needRegions bool) []CollectTarget {
	if !conn.Enabled() {
		return nil
	}
	org := conn.storageOrg()
	stored, err := conn.store.ListAccounts(org)
	if err != nil {
		return nil
	}
	targets := make([]CollectTarget, 0, len(stored))
	for _, account := range MergeAccounts(stored, conn.connector().Accounts) {
		if ctx.Err() != nil {
			return targets
		}
		health, err := conn.store.GetHealth(org, account.AccountID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			continue
		}
		if health.Status == StatusDisconnected {
			if !needRegions {
				since := health.DisconnectedSince
				if since.IsZero() {
					since = health.FailureSince
				}
				targets = append(targets, CollectTarget{AccountID: account.AccountID, Cause: lostAccessCause(health.ErrorCode, health), Since: since})
			}
			continue
		}
		regions, off := CollectRegions(account, health, conn.now().UTC())
		if needRegions {
			regions = withoutRegions(regions, off)
		}
		if needRegions && len(regions) == 0 {
			continue
		}
		credentials, err := conn.credentials(ctx, account.RoleARN, account.ExternalID, homeRegion(regions))
		if err == nil {
			err = conn.confirmIdentity(ctx, account, health, credentials, homeRegion(regions))
		}
		if err != nil {
			if !needRegions {
				code := awsauth.ErrorCode(err)
				if errors.Is(err, ErrAccountMismatch) {
					code = ErrAccountMismatch.Error()
				}
				targets = append(targets, CollectTarget{AccountID: account.AccountID,
					Cause: lostAccessCause(code, health), Since: conn.now().UTC()})
			}
			continue
		}
		targets = append(targets, CollectTarget{AccountID: account.AccountID, Regions: append([]string(nil), regions...), Credentials: credentials})
	}
	return targets
}

// CollectRegions is the one rule for which Regions an account is read in.
// regions are the account's selected Regions or, when it selects none, the
// Regions its health check found enabled; when that opt-in status is stale,
// every known Region, so one still off reads as a real gap. off are the
// regions AWS has confirmed not enabled as of now: the collectors skip them
// until a check finds them enabled or the status goes stale.
func CollectRegions(account AccountRecord, health HealthRecord, now time.Time) (regions, off []string) {
	regions = account.Regions
	if len(regions) == 0 {
		regions = enabledRegions(health.Regions)
		if len(health.Regions) > 0 && !health.regionsFresh(now) {
			regions = knownRegions(health.Regions)
		}
	}
	return append([]string(nil), regions...), health.NotEnabledRegionsAt(regions, now)
}

// confirmIdentity makes sure a config account's credentials sign in to that
// account before anything is read through them. Only a confirmation this
// process saw counts: a saved Connected record may predate a restart with
// another account's key. One STS GetCallerIdentity call (free and global,
// so it needs no region check) confirms it, as does a successful health
// check; the answer is kept until a health check fails or the account's
// role changes. Credentials of another account are recorded as
// account_mismatch, so the account disconnects and nothing is read.
func (conn *Service) confirmIdentity(ctx context.Context, account AccountRecord, health HealthRecord, credentials aws.CredentialsProvider, region string) error {
	if account.Source != SourceConfig {
		return nil
	}
	if conn.identityConfirmed(account) {
		return nil
	}
	_, accountID, err := conn.identity(ctx, credentials, region)
	if err == nil && accountID != "" && accountID != account.AccountID {
		err = ErrAccountMismatch
	}
	if err != nil {
		if errors.Is(err, ErrAccountMismatch) {
			// Re-read under the account's lock: the record passed in may be
			// older than one a health check saved meanwhile.
			unlock := conn.lockHealth(account.AccountID)
			record, readErr := conn.store.GetHealth(conn.storageOrg(), account.AccountID)
			if readErr != nil {
				record = health
			}
			record.AccountID = account.AccountID
			recordIdentityFailure(&record, err, conn.now().UTC())
			_ = conn.store.SaveHealth(conn.storageOrg(), record)
			unlock()
		}
		return err
	}
	conn.setIdentityConfirmed(account, true)
	return nil
}

func identityKey(account AccountRecord) string {
	return account.RoleARN + "\x00" + account.ExternalID
}

func (conn *Service) identityConfirmed(account AccountRecord) bool {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	key, ok := conn.confirmed[account.AccountID]
	return ok && key == identityKey(account)
}

// setIdentityConfirmed records, for this process only, whether the
// account's credentials were last seen signing in to the account.
func (conn *Service) setIdentityConfirmed(account AccountRecord, confirmed bool) {
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if confirmed {
		conn.confirmed[account.AccountID] = identityKey(account)
		return
	}
	delete(conn.confirmed, account.AccountID)
}

// lostAccessCause names why an account cannot be read now. The health check
// reports both a missing and a rejected server identity as
// no_server_identity; an account that was checked successfully before had an
// identity that worked, so its loss is credentials that stopped working.
// Credentials of another AWS account read account_mismatch.
func lostAccessCause(code string, health HealthRecord) string {
	switch {
	case code == awsauth.ErrNoServerIdentity.Error() && !health.CheckedAt.IsZero():
		return CauseCredentialsInvalid
	case code == awsauth.ErrNoServerIdentity.Error():
		return CauseNoServerIdentity
	case code == ErrAccountMismatch.Error():
		return CauseAccountMismatch
	}
	return CauseAccountDisconnected
}

// AccountIDs lists every connected account, wizard and config alike,
// whatever its health. The collectors purge stored data for any account not
// on this list.
func (conn *Service) AccountIDs(context.Context) ([]string, error) {
	if !conn.Enabled() {
		return nil, ErrDisabled
	}
	stored, err := conn.store.ListAccountsStrict(conn.storageOrg())
	if err != nil {
		return nil, err
	}
	merged := MergeAccounts(stored, conn.connector().Accounts)
	ids := make([]string, 0, len(merged))
	for _, account := range merged {
		ids = append(ids, account.AccountID)
	}
	return ids, nil
}

// syncView summarises the account's units in the snapshot store.
func (conn *Service) syncView(accountID string) AccountSyncView {
	view := AccountSyncView{State: SyncStateOff, Services: []ServiceSyncView{}}
	if conn.snapshots == nil || !conn.connector().Collect.Enable {
		return view
	}
	statuses, err := conn.snapshots.Sync(context.Background(), snapshot.Query{Account: accountID})
	if err != nil {
		return view
	}
	view.State = SyncStatePending
	if len(statuses) == 0 {
		view.FirstSync = true
		return view
	}

	type aggregate struct {
		view   ServiceSyncView
		failed int
		gaps   map[string]bool
	}
	services := make(map[string]*aggregate)
	var syncedAt time.Time
	failed := 0
	for _, status := range statuses {
		entry := services[status.Unit.Service]
		if entry == nil {
			entry = &aggregate{view: ServiceSyncView{Service: status.Unit.Service, Gaps: []string{}}, gaps: map[string]bool{}}
			services[status.Unit.Service] = entry
		}
		entry.view.Units++
		view.Units++
		// A unit in a Region that is not enabled has nothing to read. The
		// account names the Region once ("1 region not enabled"); the
		// service is neither failed nor not enabled because of it.
		regionOff := regionNotEnabled(status.Sync)
		if status.Sync.State != model.SyncPending && status.Sync.State != "" {
			entry.view.UnitsDone++
			view.UnitsDone++
		}
		if status.Sync.State == model.SyncFailed && !regionOff {
			entry.failed++
			failed++
		}
		for _, gap := range status.Sync.Gaps {
			if gap.Reason == model.GapNotEnabled {
				continue
			}
			entry.gaps[string(gap.Reason)] = true
		}
		if last := status.Sync.LastGoodAt; !last.IsZero() {
			if entry.view.LastGoodAt == nil || last.After(*entry.view.LastGoodAt) {
				copied := last
				entry.view.LastGoodAt = &copied
			}
			if last.After(syncedAt) {
				syncedAt = last
			}
		}
	}

	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := services[name]
		switch {
		case entry.view.UnitsDone < entry.view.Units && entry.view.UnitsDone == 0:
			entry.view.State = SyncStatePending
		case entry.failed > 0:
			entry.view.State = SyncStateFailed
		case entry.view.UnitsDone < entry.view.Units:
			entry.view.State = SyncStateSyncing
		default:
			entry.view.State = SyncStateOK
		}
		for reason := range entry.gaps {
			entry.view.Gaps = append(entry.view.Gaps, reason)
		}
		sort.Strings(entry.view.Gaps)
		view.Services = append(view.Services, entry.view)
	}

	if !syncedAt.IsZero() {
		view.SyncedAt = &syncedAt
	}
	view.FirstSync = view.UnitsDone < view.Units
	switch {
	case view.UnitsDone == 0:
		view.State = SyncStatePending
	case view.FirstSync:
		view.State = SyncStateSyncing
	case failed == view.Units:
		view.State = SyncStateFailed
	case failed > 0:
		view.State = SyncStatePartial
	default:
		view.State = SyncStateOK
	}
	return view
}

// regionNotEnabled reports a unit whose last attempt found its Region not
// enabled for the account.
func regionNotEnabled(sync model.SyncState) bool {
	if sync.ErrorCode != "" && model.GapReasonForCode(sync.ErrorCode) == model.GapNotEnabled {
		return true
	}
	for _, gap := range sync.Gaps {
		if gap.Reason == model.GapNotEnabled {
			return true
		}
	}
	return false
}

func withoutRegions(regions, drop []string) []string {
	if len(drop) == 0 {
		return regions
	}
	out := make([]string, 0, len(regions))
	for _, region := range regions {
		if !slices.Contains(drop, region) {
			out = append(out, region)
		}
	}
	return out
}

func knownRegions(regions []RegionStatus) []string {
	out := make([]string, 0, len(regions))
	for _, region := range regions {
		out = append(out, region.Region)
	}
	sort.Strings(out)
	return out
}
