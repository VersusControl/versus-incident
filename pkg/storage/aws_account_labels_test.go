package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// Account labels behave the same on every backend: put is an upsert,
// reads merge the read organizations with the first one winning, and a
// delete touches the write organization only.
func TestTablesParityAWSAccountLabels(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		store, ok := AWSAccountLabelsOf(backend.provider)
		if !ok {
			t.Fatalf("%s provider has no account label store", backend.name)
		}
		ctx := context.Background()
		archive := tenancy.OrgScope{Write: orgs.def}
		licensed := tenancy.OrgScope{Write: orgs.ent, Read: []string{orgs.ent, orgs.def}}
		at := time.Date(2026, 11, 22, 9, 30, 0, 123456789, time.UTC)

		mustDo(t, errOnly(store.PutAWSAccountLabel(ctx, archive, AWSAccountLabel{AccountID: "555500006666", Name: "Datadog", CreatedBy: "alice", CreatedAt: at})))
		mustDo(t, errOnly(store.PutAWSAccountLabel(ctx, archive, AWSAccountLabel{AccountID: "555500007777", Name: "old name", CreatedAt: at})))
		saved := must(store.PutAWSAccountLabel(ctx, licensed, AWSAccountLabel{AccountID: "555500007777", Name: "  log archive ", CreatedBy: "bob", CreatedAt: at}))
		if saved.Label != AWSAccountLabelKnown || saved.Name != "log archive" || !saved.CreatedAt.Equal(at.Truncate(time.Microsecond)) {
			t.Fatalf("saved = %+v", saved)
		}
		merged := must(store.AWSAccountLabels(ctx, licensed))
		if len(merged) != 2 || merged[0].Name != "Datadog" || merged[0].CreatedBy != "alice" || !merged[0].Inherited ||
			merged[1].Name != "log archive" || merged[1].Inherited {
			t.Fatalf("licensed read = %+v", merged)
		}
		record("licensed read", merged)
		record("archive read", must(store.AWSAccountLabels(ctx, archive)))

		updated := must(store.PutAWSAccountLabel(ctx, licensed, AWSAccountLabel{AccountID: "555500007777", Name: "archive", CreatedBy: "bob", CreatedAt: at.Add(time.Hour)}))
		if !updated.CreatedAt.Equal(at.Truncate(time.Microsecond)) || updated.Name != "archive" {
			t.Fatalf("update = %+v, want the first creation time kept", updated)
		}
		afterUpdate := must(store.AWSAccountLabels(ctx, licensed))
		if !afterUpdate[1].CreatedAt.Equal(at.Truncate(time.Microsecond)) {
			t.Fatalf("stored after update = %+v", afterUpdate[1])
		}
		record("after update", afterUpdate)

		record("delete own", must(store.DeleteAWSAccountLabel(ctx, licensed, "555500007777")))
		// The default org's label for the same account shows through now,
		// inherited, and cannot be deleted from the licensed org.
		if _, err := store.DeleteAWSAccountLabel(ctx, licensed, "555500007777"); !errors.Is(err, ErrAWSAccountLabelInherited) {
			t.Fatalf("delete of the label that shows through = %v", err)
		}
		record("delete missing", must(store.DeleteAWSAccountLabel(ctx, licensed, "555500008888")))
		if deleted, err := store.DeleteAWSAccountLabel(ctx, licensed, "555500006666"); deleted || !errors.Is(err, ErrAWSAccountLabelInherited) {
			t.Fatalf("delete inherited = %v, %v", deleted, err)
		}
		record("archive own read", must(store.AWSAccountLabels(ctx, archive)))
		remaining := must(store.AWSAccountLabels(ctx, licensed))
		if len(remaining) != 2 || remaining[1].Name != "old name" {
			t.Fatalf("after delete = %+v", remaining)
		}
		record("after delete", remaining)

		for name, label := range map[string]AWSAccountLabel{
			"short account": {AccountID: "5555"},
			"other label":   {AccountID: "555500006666", Label: "connected"},
			"long name":     {AccountID: "555500006666", Name: strings.Repeat("x", MaxAWSAccountLabelName+1)},
			"control":       {AccountID: "555500006666", Name: "a\nb"},
		} {
			if _, err := store.PutAWSAccountLabel(ctx, licensed, label); !errors.Is(err, ErrInvalidAWSAccountLabel) {
				t.Errorf("%s: err = %v", name, err)
			}
		}
		if _, err := store.DeleteAWSAccountLabel(ctx, licensed, "not-an-id"); !errors.Is(err, ErrInvalidAWSAccountLabel) {
			t.Errorf("delete bad id: err = %v", err)
		}
	})
}

func errOnly[T any](_ T, err error) error { return err }
