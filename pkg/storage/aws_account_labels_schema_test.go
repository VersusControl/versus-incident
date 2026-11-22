package storage

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// The account label migration is schema only, and its table keys one row
// per organization and account: a second row for the same pair is refused,
// the same account under another organization is its own row, and a row
// with no time is refused.
func TestAWSAccountLabelsMigrationSchema(t *testing.T) {
	text, err := os.ReadFile("migrations/041_aws_account_labels.sql")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE)\b`).Match(stripSQLComments(string(text))) {
		t.Fatalf("migration carries a data statement:\n%s", text)
	}
	orgs := newTablesOrgs()
	for _, backend := range tablesBackends(t) {
		if backend.raw == nil {
			continue
		}
		t.Run(backend.name, func(t *testing.T) {
			insert := func(org, account, created string) error {
				_, err := backend.raw.Exec(`INSERT INTO aws_account_labels (org_id, account_id, label, created_at) VALUES ('` + org + `', '` +
					account + `', 'known', ` + created + `)`)
				return err
			}
			at := "'2026-11-22T10:00:00Z'"
			if err := insert(orgs.ent, "555500006666", at); err != nil {
				t.Fatal(err)
			}
			if err := insert(orgs.ent, "555500006666", at); err == nil {
				t.Fatal("a second row for one organization and account was stored")
			}
			if err := insert(orgs.def, "555500006666", at); err != nil {
				t.Fatalf("same account under another organization: %v", err)
			}
			if err := insert(orgs.ent, "555500007777", "NULL"); err == nil {
				t.Fatal("a row with no time was stored")
			}
			var name, createdBy string
			if err := backend.raw.QueryRow(`SELECT name, created_by FROM aws_account_labels WHERE org_id = '`+orgs.def+`'`).Scan(&name, &createdBy); err != nil {
				t.Fatal(err)
			}
			if name != "" || createdBy != "" {
				t.Fatalf("defaults = %q %q", name, createdBy)
			}
			if backend.postgres {
				var columns int
				if err := backend.raw.QueryRow(`SELECT count(*) FROM information_schema.key_column_usage k
 JOIN information_schema.table_constraints c ON c.constraint_name = k.constraint_name AND c.table_name = k.table_name
 WHERE k.table_name = 'aws_account_labels' AND c.constraint_type = 'PRIMARY KEY' AND k.column_name IN ('org_id', 'account_id')`).Scan(&columns); err != nil {
					t.Fatal(err)
				}
				if columns != 2 {
					t.Fatalf("primary key columns = %d, want org_id and account_id", columns)
				}
			}
		})
	}
}

func stripSQLComments(text string) []byte {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if code, _, _ := strings.Cut(line, "--"); strings.TrimSpace(code) != "" {
			kept = append(kept, code)
		}
	}
	return []byte(strings.Join(kept, "\n"))
}

// The name limit counts characters, not bytes, on every backend, and a
// name holding NUL or invalid UTF-8 is refused before it reaches a table.
func TestTablesParityAWSAccountLabelNameLimits(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		store, _ := AWSAccountLabelsOf(backend.provider)
		ctx := context.Background()
		scope := tenancy.OrgScope{Write: orgs.ent, Read: []string{orgs.ent}}
		at := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
		atLimit := strings.Repeat("界", MaxAWSAccountLabelName)
		saved := must(store.PutAWSAccountLabel(ctx, scope, AWSAccountLabel{AccountID: "555500006666", Name: atLimit, CreatedAt: at}))
		if saved.Name != atLimit {
			t.Fatalf("saved = %+v", saved)
		}
		record("at limit", must(store.AWSAccountLabels(ctx, scope)))
		for name, label := range map[string]AWSAccountLabel{
			"over limit":   {AccountID: "555500006666", Name: atLimit + "界"},
			"nul":          {AccountID: "555500006666", Name: "a\x00b"},
			"invalid utf8": {AccountID: "555500006666", Name: "a\xffb"},
			"del":          {AccountID: "555500006666", Name: "a\x7fb"},
			"nul by":       {AccountID: "555500006666", Name: "ok", CreatedBy: "a\x00b"},
		} {
			if _, err := store.PutAWSAccountLabel(ctx, scope, label); !errors.Is(err, ErrInvalidAWSAccountLabel) {
				t.Errorf("%s: err = %v", name, err)
			}
		}
		record("after refused", must(store.AWSAccountLabels(ctx, scope)))
	})
}
