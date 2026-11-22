package storage

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// AWSAccountLabel is the label a user gave one AWS account: who it is
// ("Datadog", "our log archive") and when, by whom. The labels a deployment
// derives itself (connected, same organization) are never stored.
type AWSAccountLabel struct {
	AccountID string    `json:"account_id"`
	Label     string    `json:"label"`
	Name      string    `json:"name,omitempty"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Inherited marks a label read from an organization the deployment
	// reads but does not write (the open-source default org after a
	// licensed upgrade). It cannot be deleted from here; setting a label
	// for the account overrides it.
	Inherited bool `json:"inherited,omitempty"`
}

// AWSAccountLabelKnown is the one label a user sets: the account is known.
const AWSAccountLabelKnown = "known"

// MaxAWSAccountLabelName bounds a label's name, in characters.
const MaxAWSAccountLabelName = 128

// ErrAWSAccountLabelInherited is a delete of a label the deployment only
// inherits from an organization it reads but does not write.
var ErrAWSAccountLabelInherited = errors.New("storage: aws account label is inherited")

// ErrInvalidAWSAccountLabel is a label with a malformed account ID, an
// unknown label, or a name that is too long or holds control characters.
var ErrInvalidAWSAccountLabel = errors.New("storage: invalid aws account label")

var awsAccountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)

// AWSAccountLabelStore keeps the account labels. Reads merge the read scope
// in order: the first organization that labelled an account wins, and a
// label from any organization but the write one is marked inherited. Writes
// and deletes touch the write organization only; an update keeps the time
// the label was first created, and deleting a label that is only inherited
// is refused with ErrAWSAccountLabelInherited. It is optional beside
// AWSInventoryStore; every backend with the inventory tables has it.
type AWSAccountLabelStore interface {
	AWSAccountLabels(ctx context.Context, scope tenancy.OrgScope) ([]AWSAccountLabel, error)
	PutAWSAccountLabel(ctx context.Context, scope tenancy.OrgScope, label AWSAccountLabel) (AWSAccountLabel, error)
	DeleteAWSAccountLabel(ctx context.Context, scope tenancy.OrgScope, accountID string) (bool, error)
}

var (
	_ AWSAccountLabelStore = (*sqlTables)(nil)
	_ AWSAccountLabelStore = (*memoryTables)(nil)
)

// AWSAccountLabelsOf returns the provider's account label store, if it
// has one.
func AWSAccountLabelsOf(provider Provider) (AWSAccountLabelStore, bool) {
	store, ok := provider.(AWSAccountLabelStore)
	return store, ok
}

// cleanAWSAccountLabel validates a label for writing. An empty label is
// known; the time is stored at table precision.
func cleanAWSAccountLabel(label AWSAccountLabel) (AWSAccountLabel, error) {
	label.Name = strings.TrimSpace(label.Name)
	if label.Label == "" {
		label.Label = AWSAccountLabelKnown
	}
	if !awsAccountIDPattern.MatchString(label.AccountID) || label.Label != AWSAccountLabelKnown ||
		utf8.RuneCountInString(label.Name) > MaxAWSAccountLabelName || !validTableStrings(label.Name, label.CreatedBy) {
		return AWSAccountLabel{}, ErrInvalidAWSAccountLabel
	}
	for _, r := range label.Name {
		if r < 0x20 || r == 0x7f {
			return AWSAccountLabel{}, ErrInvalidAWSAccountLabel
		}
	}
	if label.CreatedAt.IsZero() {
		label.CreatedAt = time.Now()
	}
	label.CreatedAt = tableTime(label.CreatedAt)
	return label, nil
}

func sortAWSAccountLabels(labels []AWSAccountLabel) {
	sort.Slice(labels, func(left, right int) bool { return labels[left].AccountID < labels[right].AccountID })
}

// mergeAWSAccountLabels keeps, per account, the label of the first
// organization in read order, marking one from another organization than
// write as inherited.
func mergeAWSAccountLabels(write string, orgs []string, byOrg map[string][]AWSAccountLabel) []AWSAccountLabel {
	seen := map[string]bool{}
	out := make([]AWSAccountLabel, 0)
	for _, org := range orgs {
		for _, label := range byOrg[org] {
			if seen[label.AccountID] {
				continue
			}
			seen[label.AccountID] = true
			label.Inherited = org != write
			out = append(out, label)
		}
	}
	sortAWSAccountLabels(out)
	return out
}

// ---- SQL (Postgres and the local SQLite tables) ----

func (t *sqlTables) AWSAccountLabels(ctx context.Context, scope tenancy.OrgScope) ([]AWSAccountLabel, error) {
	db, err := t.tablesDB()
	if err != nil {
		return nil, err
	}
	scope = inventoryScope(scope)
	rows, err := db.QueryContext(ctx, t.q(`SELECT org_id, account_id, label, name, created_by, created_at FROM aws_account_labels
 WHERE org_id IN (`+placeholders(len(scope.Read))+`) ORDER BY account_id`), stringArgs(scope.Read)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byOrg := map[string][]AWSAccountLabel{}
	for rows.Next() {
		var org string
		var label AWSAccountLabel
		var created tableTimeValue
		if err := rows.Scan(&org, &label.AccountID, &label.Label, &label.Name, &label.CreatedBy, &created); err != nil {
			return nil, err
		}
		label.CreatedAt = created.Time
		byOrg[org] = append(byOrg[org], label)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return mergeAWSAccountLabels(scope.Write, scope.Read, byOrg), nil
}

func (t *sqlTables) PutAWSAccountLabel(ctx context.Context, scope tenancy.OrgScope, label AWSAccountLabel) (AWSAccountLabel, error) {
	label, err := cleanAWSAccountLabel(label)
	if err != nil {
		return AWSAccountLabel{}, err
	}
	scope = inventoryScope(scope)
	err = t.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, t.q(`INSERT INTO aws_account_labels (org_id, account_id, label, name, created_by, created_at)
 VALUES (?, ?, ?, ?, ?, ?)
 ON CONFLICT (org_id, account_id) DO UPDATE SET label = excluded.label, name = excluded.name, created_by = excluded.created_by`),
			scope.Write, label.AccountID, label.Label, label.Name, label.CreatedBy, t.at(label.CreatedAt)); err != nil {
			return err
		}
		var created tableTimeValue
		if err := tx.QueryRowContext(ctx, t.q(`SELECT created_at FROM aws_account_labels WHERE org_id = ? AND account_id = ?`),
			scope.Write, label.AccountID).Scan(&created); err != nil {
			return err
		}
		label.CreatedAt = created.Time
		return nil
	})
	if err != nil {
		return AWSAccountLabel{}, err
	}
	return label, nil
}

func (t *sqlTables) DeleteAWSAccountLabel(ctx context.Context, scope tenancy.OrgScope, accountID string) (bool, error) {
	if !awsAccountIDPattern.MatchString(accountID) {
		return false, ErrInvalidAWSAccountLabel
	}
	scope = inventoryScope(scope)
	deleted := false
	err := t.write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, t.q(`DELETE FROM aws_account_labels WHERE org_id = ? AND account_id = ?`), scope.Write, accountID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count > 0 {
			deleted = count > 0
			return err
		}
		var inherited int
		if err := tx.QueryRowContext(ctx, t.q(`SELECT COUNT(*) FROM aws_account_labels WHERE org_id IN (`+placeholders(len(scope.Read))+`) AND account_id = ?`),
			append(stringArgs(scope.Read), accountID)...).Scan(&inherited); err != nil {
			return err
		}
		if inherited > 0 {
			return ErrAWSAccountLabelInherited
		}
		return nil
	})
	return deleted, err
}

// ---- Memory ----

func (t *memoryTables) AWSAccountLabels(_ context.Context, scope tenancy.OrgScope) ([]AWSAccountLabel, error) {
	scope = inventoryScope(scope)
	t.mu.RLock()
	defer t.mu.RUnlock()
	byOrg := map[string][]AWSAccountLabel{}
	for _, org := range scope.Read {
		for _, label := range t.accountLabels[org] {
			byOrg[org] = append(byOrg[org], label)
		}
	}
	return mergeAWSAccountLabels(scope.Write, scope.Read, byOrg), nil
}

func (t *memoryTables) PutAWSAccountLabel(_ context.Context, scope tenancy.OrgScope, label AWSAccountLabel) (AWSAccountLabel, error) {
	label, err := cleanAWSAccountLabel(label)
	if err != nil {
		return AWSAccountLabel{}, err
	}
	scope = inventoryScope(scope)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.accountLabels == nil {
		t.accountLabels = map[string]map[string]AWSAccountLabel{}
	}
	if t.accountLabels[scope.Write] == nil {
		t.accountLabels[scope.Write] = map[string]AWSAccountLabel{}
	}
	if existing, ok := t.accountLabels[scope.Write][label.AccountID]; ok {
		label.CreatedAt = existing.CreatedAt
	}
	t.accountLabels[scope.Write][label.AccountID] = label
	return label, nil
}

func (t *memoryTables) DeleteAWSAccountLabel(_ context.Context, scope tenancy.OrgScope, accountID string) (bool, error) {
	if !awsAccountIDPattern.MatchString(accountID) {
		return false, ErrInvalidAWSAccountLabel
	}
	scope = inventoryScope(scope)
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.accountLabels[scope.Write][accountID]; !ok {
		for _, org := range scope.Read {
			if _, ok := t.accountLabels[org][accountID]; ok {
				return false, ErrAWSAccountLabelInherited
			}
		}
		return false, nil
	}
	delete(t.accountLabels[scope.Write], accountID)
	return true, nil
}
