package storage

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// Group correction kinds.
const (
	// AWSGroupRename gives group GroupID the name Name.
	AWSGroupRename = "rename"
	// AWSGroupMerge folds group GroupID into group TargetGroupID. A merge
	// whose target is GroupID itself keeps the group as it is: it is how a
	// deployment undoes a merge it inherits.
	AWSGroupMerge = "merge"
	// AWSGroupMove puts the resource MemberARN into group TargetGroupID.
	AWSGroupMove = "move"
)

// Bounds on a group correction.
const (
	// MaxAWSGroupName bounds a group's new name, in characters.
	MaxAWSGroupName = 128
	// MaxAWSGroupID bounds a group ID, in bytes.
	MaxAWSGroupID = 512
	// MaxAWSGroupMemberARN bounds a moved resource's ARN, in bytes.
	MaxAWSGroupMemberARN = 2048
	// MaxAWSGroupCorrectionsPerAccount bounds how many corrections one
	// account holds in the write organization.
	MaxAWSGroupCorrectionsPerAccount = 2000
)

// AWSGroupCorrection is one correction a user made to the resource groups of
// one account: a rename, a merge or a move. It keys on stable group IDs and
// on the member ARN, never on a position or a computed name, so it survives
// re-reads; one whose group is gone is reported, never re-aimed.
type AWSGroupCorrection struct {
	AccountID     string    `json:"account_id"`
	Kind          string    `json:"kind"`
	GroupID       string    `json:"group_id,omitempty"`
	MemberARN     string    `json:"member_arn,omitempty"`
	TargetGroupID string    `json:"target_group_id,omitempty"`
	Name          string    `json:"name,omitempty"`
	CreatedBy     string    `json:"created_by,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	// Inherited marks a correction read from an organization the
	// deployment reads but does not write. It cannot be deleted from here;
	// a correction with the same key overrides it.
	Inherited bool `json:"inherited,omitempty"`
	// ExpectedCreatedAt, on a put, is the creation time of the correction
	// with the same key the writer read: the put replaces it only while it
	// is still that correction, and answers ErrAWSGroupCorrectionConflict
	// otherwise (it changed, or it is gone). Nil writes unconditionally.
	ExpectedCreatedAt *time.Time `json:"-"`
}

// AWSGroupCorrectionKey identifies one correction: one rename or merge per
// group, one move per member.
type AWSGroupCorrectionKey struct {
	AccountID string `json:"account_id"`
	Kind      string `json:"kind"`
	GroupID   string `json:"group_id,omitempty"`
	MemberARN string `json:"member_arn,omitempty"`
}

// Key returns the correction's key.
func (c AWSGroupCorrection) Key() AWSGroupCorrectionKey {
	return AWSGroupCorrectionKey{AccountID: c.AccountID, Kind: c.Kind, GroupID: c.GroupID, MemberARN: c.MemberARN}
}

// ErrInvalidAWSGroupCorrection is a correction with a malformed account,
// kind, group ID, ARN or name, or fields its kind does not take.
var ErrInvalidAWSGroupCorrection = errors.New("storage: invalid aws group correction")

// ErrAWSGroupCorrectionInherited is a delete of a correction the deployment
// only inherits from an organization it reads but does not write.
var ErrAWSGroupCorrectionInherited = errors.New("storage: aws group correction is inherited")

// ErrAWSGroupCorrectionConflict is a put whose ExpectedCreatedAt no longer
// matches the stored correction with its key: someone else changed or
// removed it since the writer read it.
var ErrAWSGroupCorrectionConflict = errors.New("storage: aws group correction changed since it was read")

// expectedCorrection checks a put's ExpectedCreatedAt against the stored
// correction with its key, found or not.
func expectedCorrection(expected *time.Time, found bool, created time.Time) error {
	if expected == nil {
		return nil
	}
	if !found || !tableTime(*expected).Equal(tableTime(created)) {
		return ErrAWSGroupCorrectionConflict
	}
	return nil
}

// ErrAWSGroupCorrectionLimit is a new correction for an account that already
// holds MaxAWSGroupCorrectionsPerAccount.
var ErrAWSGroupCorrectionLimit = errors.New("storage: too many aws group corrections for the account")

// awsGroupCorrectionLimit is the per-account bound in force; tests lower it.
var awsGroupCorrectionLimit = MaxAWSGroupCorrectionsPerAccount

// AWSGroupCorrectionStore keeps the group corrections. Reads merge the read
// scope in order: per key the first organization wins, and a correction
// from any organization but the write one is marked inherited. Reads come
// in the order the corrections were made. Writes and deletes touch the
// write organization only; a put with an existing key replaces it, as a new
// correction made now. It is optional beside AWSInventoryStore; every
// backend with the inventory tables has it.
type AWSGroupCorrectionStore interface {
	// AWSGroupCorrections lists the corrections of one account, or of every
	// account when accountID is empty.
	AWSGroupCorrections(ctx context.Context, scope tenancy.OrgScope, accountID string) ([]AWSGroupCorrection, error)
	PutAWSGroupCorrection(ctx context.Context, scope tenancy.OrgScope, correction AWSGroupCorrection) (AWSGroupCorrection, error)
	DeleteAWSGroupCorrection(ctx context.Context, scope tenancy.OrgScope, key AWSGroupCorrectionKey) (bool, error)
}

var (
	_ AWSGroupCorrectionStore = (*sqlTables)(nil)
	_ AWSGroupCorrectionStore = (*memoryTables)(nil)
)

// AWSGroupCorrectionsOf returns the provider's group correction store, if
// it has one.
func AWSGroupCorrectionsOf(provider Provider) (AWSGroupCorrectionStore, bool) {
	store, ok := provider.(AWSGroupCorrectionStore)
	return store, ok
}

// awsMemberARNPattern is an ARN whose account field is empty (S3, a few
// global services) or 12 digits, with no space anywhere.
var awsMemberARNPattern = regexp.MustCompile(`^arn:[a-z0-9-]+:[a-z0-9-]+:[a-z0-9-]*:([0-9]{12})?:[^\s]+$`)

// validAWSGroupID accepts a group ID the grouping can produce: printable
// text up to MaxAWSGroupID bytes.
func validAWSGroupID(id string) bool {
	return id != "" && len(id) <= MaxAWSGroupID && validTableKey(id) && utf8.ValidString(id) && strings.TrimSpace(id) == id
}

// validAWSMemberARN accepts a resource ARN in the account, or one with no
// account field.
func validAWSMemberARN(arn, account string) bool {
	if len(arn) > MaxAWSGroupMemberARN || !utf8.ValidString(arn) || !validTableKey(arn) {
		return false
	}
	match := awsMemberARNPattern.FindStringSubmatch(arn)
	return match != nil && (match[1] == "" || match[1] == account)
}

// printableName accepts a name a user sees: no control characters
// (unicode.Cc), no format characters that hide or reorder text
// (unicode.Cf: bidirectional controls, zero-width characters, the soft
// hyphen, the byte order mark, interlinear annotations, the tag block) and
// no line or paragraph separators.
func printableName(name string) bool {
	for _, r := range name {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return false
		}
	}
	return true
}

// AWSUserGroupPrefix starts the ID of a group a user makes by moving a
// resource into it; the rest of the ID is the group's name.
const AWSUserGroupPrefix = "user:"

// validTargetGroupID accepts a target group ID; one a user makes carries a
// name, held to the rules of a group name.
func validTargetGroupID(id string) bool {
	if !validAWSGroupID(id) {
		return false
	}
	if name, ok := strings.CutPrefix(id, AWSUserGroupPrefix); ok {
		name = strings.TrimSpace(name)
		return name != "" && utf8.RuneCountInString(name) <= MaxAWSGroupName && printableName(name)
	}
	return true
}

// validAWSGroupCorrectionKey checks a key's shape: each kind sets exactly
// the fields it is keyed on.
func validAWSGroupCorrectionKey(key AWSGroupCorrectionKey) bool {
	if !awsAccountIDPattern.MatchString(key.AccountID) {
		return false
	}
	switch key.Kind {
	case AWSGroupRename, AWSGroupMerge:
		return validAWSGroupID(key.GroupID) && key.MemberARN == ""
	case AWSGroupMove:
		return key.GroupID == "" && validAWSMemberARN(key.MemberARN, key.AccountID)
	}
	return false
}

// cleanAWSGroupCorrection validates a correction for writing. The name is
// trimmed and the time stored at table precision.
func cleanAWSGroupCorrection(correction AWSGroupCorrection) (AWSGroupCorrection, error) {
	correction.Name = strings.TrimSpace(correction.Name)
	if !validAWSGroupCorrectionKey(correction.Key()) || !validTableStrings(correction.Name, correction.CreatedBy) ||
		len(correction.CreatedBy) > 512 {
		return AWSGroupCorrection{}, ErrInvalidAWSGroupCorrection
	}
	switch correction.Kind {
	case AWSGroupRename:
		if correction.Name == "" || utf8.RuneCountInString(correction.Name) > MaxAWSGroupName || !printableName(correction.Name) ||
			correction.TargetGroupID != "" {
			return AWSGroupCorrection{}, ErrInvalidAWSGroupCorrection
		}
	case AWSGroupMerge:
		// A merge whose target is its own group is the neutral row that
		// undoes an inherited merge of that group.
		if !validTargetGroupID(correction.TargetGroupID) || correction.Name != "" {
			return AWSGroupCorrection{}, ErrInvalidAWSGroupCorrection
		}
	case AWSGroupMove:
		if !validTargetGroupID(correction.TargetGroupID) || correction.Name != "" {
			return AWSGroupCorrection{}, ErrInvalidAWSGroupCorrection
		}
	}
	if correction.CreatedAt.IsZero() {
		correction.CreatedAt = time.Now()
	}
	correction.CreatedAt = tableTime(correction.CreatedAt)
	correction.Inherited = false
	return correction, nil
}

// sortAWSGroupCorrections orders corrections as they were made, then by key
// so equal times read the same on every backend.
func sortAWSGroupCorrections(corrections []AWSGroupCorrection) {
	sort.Slice(corrections, func(left, right int) bool {
		a, b := corrections[left], corrections[right]
		switch {
		case !a.CreatedAt.Equal(b.CreatedAt):
			return a.CreatedAt.Before(b.CreatedAt)
		case a.AccountID != b.AccountID:
			return a.AccountID < b.AccountID
		case a.Kind != b.Kind:
			return a.Kind < b.Kind
		case a.GroupID != b.GroupID:
			return a.GroupID < b.GroupID
		}
		return a.MemberARN < b.MemberARN
	})
}

// mergeAWSGroupCorrections keeps, per key, the correction of the first
// organization in read order, marking one from another organization than
// write as inherited.
func mergeAWSGroupCorrections(write string, orgs []string, byOrg map[string][]AWSGroupCorrection) []AWSGroupCorrection {
	seen := map[AWSGroupCorrectionKey]bool{}
	out := make([]AWSGroupCorrection, 0)
	for _, org := range orgs {
		for _, correction := range byOrg[org] {
			if seen[correction.Key()] {
				continue
			}
			seen[correction.Key()] = true
			correction.Inherited = org != write
			out = append(out, correction)
		}
	}
	sortAWSGroupCorrections(out)
	return out
}

// ---- SQL (Postgres and the local SQLite tables) ----

func (t *sqlTables) AWSGroupCorrections(ctx context.Context, scope tenancy.OrgScope, accountID string) ([]AWSGroupCorrection, error) {
	if accountID != "" && !awsAccountIDPattern.MatchString(accountID) {
		return nil, ErrInvalidAWSGroupCorrection
	}
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	scope = inventoryScope(scope)
	query := `SELECT org_id, account_id, kind, group_id, member_arn, target_group_id, name, created_by, created_at FROM aws_group_corrections
 WHERE org_id IN (` + placeholders(len(scope.Read)) + `)`
	args := stringArgs(scope.Read)
	if accountID != "" {
		query += ` AND account_id = ?`
		args = append(args, accountID)
	}
	rows, err := db.QueryContext(ctx, t.q(query+` ORDER BY created_at, account_id, kind, group_id, member_arn`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byOrg := map[string][]AWSGroupCorrection{}
	for rows.Next() {
		var org string
		var correction AWSGroupCorrection
		var created tableTimeValue
		if err := rows.Scan(&org, &correction.AccountID, &correction.Kind, &correction.GroupID, &correction.MemberARN,
			&correction.TargetGroupID, &correction.Name, &correction.CreatedBy, &created); err != nil {
			return nil, err
		}
		correction.CreatedAt = created.Time
		byOrg[org] = append(byOrg[org], correction)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return mergeAWSGroupCorrections(scope.Write, scope.Read, byOrg), nil
}

func (t *sqlTables) PutAWSGroupCorrection(ctx context.Context, scope tenancy.OrgScope, correction AWSGroupCorrection) (AWSGroupCorrection, error) {
	correction, err := cleanAWSGroupCorrection(correction)
	if err != nil {
		return AWSGroupCorrection{}, err
	}
	scope = inventoryScope(scope)
	err = t.write(ctx, func(tx *sql.Tx) error {
		if t.postgres {
			// Two replicas adding the account's last free correction at
			// once must not both pass the count: the lock serializes the
			// count and the insert per organization and account until the
			// transaction ends.
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
				"aws_group_corrections/"+scope.Write+"/"+correction.AccountID); err != nil {
				return err
			}
		}
		if correction.ExpectedCreatedAt != nil {
			found, created := false, time.Time{}
			for _, org := range scope.Read {
				var value tableTimeValue
				err := tx.QueryRowContext(ctx, t.q(`SELECT created_at FROM aws_group_corrections
 WHERE org_id = ? AND account_id = ? AND kind = ? AND group_id = ? AND member_arn = ?`),
					org, correction.AccountID, correction.Kind, correction.GroupID, correction.MemberARN).Scan(&value)
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				found, created = true, value.Time
				break
			}
			if err := expectedCorrection(correction.ExpectedCreatedAt, found, created); err != nil {
				return err
			}
		}
		var existing, held int
		if err := tx.QueryRowContext(ctx, t.q(`SELECT COUNT(*) FROM aws_group_corrections
 WHERE org_id = ? AND account_id = ? AND kind = ? AND group_id = ? AND member_arn = ?`),
			scope.Write, correction.AccountID, correction.Kind, correction.GroupID, correction.MemberARN).Scan(&existing); err != nil {
			return err
		}
		if existing == 0 {
			if err := tx.QueryRowContext(ctx, t.q(`SELECT COUNT(*) FROM aws_group_corrections WHERE org_id = ? AND account_id = ?`),
				scope.Write, correction.AccountID).Scan(&held); err != nil {
				return err
			}
			if held >= awsGroupCorrectionLimit {
				return ErrAWSGroupCorrectionLimit
			}
		}
		_, err := tx.ExecContext(ctx, t.q(`INSERT INTO aws_group_corrections
 (org_id, account_id, kind, group_id, member_arn, target_group_id, name, created_by, created_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT (org_id, account_id, kind, group_id, member_arn) DO UPDATE SET
 target_group_id = excluded.target_group_id, name = excluded.name, created_by = excluded.created_by, created_at = excluded.created_at`),
			scope.Write, correction.AccountID, correction.Kind, correction.GroupID, correction.MemberARN,
			correction.TargetGroupID, correction.Name, correction.CreatedBy, t.at(correction.CreatedAt))
		return err
	})
	if err != nil {
		return AWSGroupCorrection{}, err
	}
	correction.ExpectedCreatedAt = nil
	return correction, nil
}

func (t *sqlTables) DeleteAWSGroupCorrection(ctx context.Context, scope tenancy.OrgScope, key AWSGroupCorrectionKey) (bool, error) {
	if !validAWSGroupCorrectionKey(key) {
		return false, ErrInvalidAWSGroupCorrection
	}
	scope = inventoryScope(scope)
	deleted := false
	err := t.write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, t.q(`DELETE FROM aws_group_corrections
 WHERE org_id = ? AND account_id = ? AND kind = ? AND group_id = ? AND member_arn = ?`),
			scope.Write, key.AccountID, key.Kind, key.GroupID, key.MemberARN)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count > 0 {
			deleted = count > 0
			return err
		}
		var inherited int
		if err := tx.QueryRowContext(ctx, t.q(`SELECT COUNT(*) FROM aws_group_corrections WHERE org_id IN (`+placeholders(len(scope.Read))+`)
 AND account_id = ? AND kind = ? AND group_id = ? AND member_arn = ?`),
			append(stringArgs(scope.Read), key.AccountID, key.Kind, key.GroupID, key.MemberARN)...).Scan(&inherited); err != nil {
			return err
		}
		if inherited > 0 {
			return ErrAWSGroupCorrectionInherited
		}
		return nil
	})
	return deleted, err
}

// ---- Memory ----

func (t *memoryTables) AWSGroupCorrections(_ context.Context, scope tenancy.OrgScope, accountID string) ([]AWSGroupCorrection, error) {
	if accountID != "" && !awsAccountIDPattern.MatchString(accountID) {
		return nil, ErrInvalidAWSGroupCorrection
	}
	scope = inventoryScope(scope)
	t.mu.RLock()
	defer t.mu.RUnlock()
	byOrg := map[string][]AWSGroupCorrection{}
	for _, org := range scope.Read {
		for _, correction := range t.groupCorrections[org] {
			if accountID == "" || correction.AccountID == accountID {
				byOrg[org] = append(byOrg[org], correction)
			}
		}
		sortAWSGroupCorrections(byOrg[org])
	}
	return mergeAWSGroupCorrections(scope.Write, scope.Read, byOrg), nil
}

func (t *memoryTables) PutAWSGroupCorrection(_ context.Context, scope tenancy.OrgScope, correction AWSGroupCorrection) (AWSGroupCorrection, error) {
	correction, err := cleanAWSGroupCorrection(correction)
	if err != nil {
		return AWSGroupCorrection{}, err
	}
	scope = inventoryScope(scope)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.groupCorrections == nil {
		t.groupCorrections = map[string]map[AWSGroupCorrectionKey]AWSGroupCorrection{}
	}
	if correction.ExpectedCreatedAt != nil {
		found, created := false, time.Time{}
		for _, org := range scope.Read {
			if stored, ok := t.groupCorrections[org][correction.Key()]; ok {
				found, created = true, stored.CreatedAt
				break
			}
		}
		if err := expectedCorrection(correction.ExpectedCreatedAt, found, created); err != nil {
			return AWSGroupCorrection{}, err
		}
	}
	own := t.groupCorrections[scope.Write]
	if own == nil {
		own = map[AWSGroupCorrectionKey]AWSGroupCorrection{}
		t.groupCorrections[scope.Write] = own
	}
	if _, ok := own[correction.Key()]; !ok {
		held := 0
		for key := range own {
			if key.AccountID == correction.AccountID {
				held++
			}
		}
		if held >= awsGroupCorrectionLimit {
			return AWSGroupCorrection{}, ErrAWSGroupCorrectionLimit
		}
	}
	correction.ExpectedCreatedAt = nil
	own[correction.Key()] = correction
	return correction, nil
}

func (t *memoryTables) DeleteAWSGroupCorrection(_ context.Context, scope tenancy.OrgScope, key AWSGroupCorrectionKey) (bool, error) {
	if !validAWSGroupCorrectionKey(key) {
		return false, ErrInvalidAWSGroupCorrection
	}
	scope = inventoryScope(scope)
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.groupCorrections[scope.Write][key]; !ok {
		for _, org := range scope.Read {
			if _, ok := t.groupCorrections[org][key]; ok {
				return false, ErrAWSGroupCorrectionInherited
			}
		}
		return false, nil
	}
	delete(t.groupCorrections[scope.Write], key)
	return true, nil
}
