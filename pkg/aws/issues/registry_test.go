package issues

import (
	"errors"
	"slices"
	"testing"
)

// frozenRuleIDs is every rule ID an issue has ever carried. A rule ID is
// hashed into its issues' IDs and stored with every rejection, so an ID is
// never removed from this list: a rename keeps the old ID through
// Registry.Alias, and both stay here.
var frozenRuleIDs = []string{}

// The registered rule IDs match the frozen list exactly: a new rule adds
// its ID here, and a rename that drops an ID without an alias fails.
func TestRuleIDsAreFrozen(t *testing.T) {
	if got := Default.IDs(); !slices.Equal(got, frozenRuleIDs) {
		t.Fatalf("rule IDs = %q, frozen = %q; a renamed rule needs Registry.Alias from its old ID", got, frozenRuleIDs)
	}
	for _, id := range frozenRuleIDs {
		if _, ok := Default.Lookup(id); !ok {
			t.Fatalf("frozen rule ID %q no longer resolves", id)
		}
	}
}

// A renamed rule keeps its issues' IDs, and with them their rejections,
// through any number of renames.
func TestRenamedRuleKeepsIssueIDs(t *testing.T) {
	first := NewRegistry()
	if err := first.Register(&testRule{id: "public-bucket", kind: KindSecurity}); err != nil {
		t.Fatal(err)
	}
	renamed := NewRegistry()
	if err := renamed.Register(&testRule{id: "bucket-public-access", kind: KindSecurity}); err != nil {
		t.Fatal(err)
	}
	if err := renamed.Alias("bucket-public", "bucket-public-access"); err != nil {
		t.Fatal(err)
	}
	if err := renamed.Alias("public-bucket", "bucket-public"); err != nil {
		t.Fatal(err)
	}
	if got := renamed.StableID("bucket-public-access"); got != "public-bucket" {
		t.Fatalf("stable ID = %q", got)
	}
	if IssueID(renamed.StableID("bucket-public-access"), bucketARN) != IssueID(first.StableID("public-bucket"), bucketARN) {
		t.Fatal("a rename changed the issue ID")
	}
	if rule, ok := renamed.Lookup("public-bucket"); !ok || rule.ID() != "bucket-public-access" {
		t.Fatalf("old ID resolves to %v, %v", rule, ok)
	}
	if got := renamed.IDs(); !slices.Equal(got, []string{"bucket-public", "bucket-public-access", "public-bucket"}) {
		t.Fatalf("IDs = %q", got)
	}

	// The evaluation matches the stored issue under its old rule ID and
	// keeps its rejection.
	h := newHistory(t, start)
	h.cycle(complete(bucketFinding("*", SeverityHigh)))
	h.reject("public on purpose: docs site", "")
	outcome := complete(bucketFinding("*", SeverityHigh))
	outcome.Rule, outcome.StableID, outcome.Names = "bucket-public-access", renamed.StableID("bucket-public-access"), renamed.names("bucket-public-access")
	h.cycle(outcome)
	if issue := h.only(); issue.State != StateRejected || issue.Rule != "bucket-public-access" {
		t.Fatalf("after the rename: %+v", issue)
	}
	outcome.Findings = nil
	h.cycle(outcome)
	if h.only().State != StateResolved {
		t.Fatalf("the renamed rule does not own its old issue: %+v", h.only())
	}

	for name, err := range map[string]error{
		"bad id":         renamed.Register(&testRule{id: "Bad ID", kind: KindSecurity}),
		"bad kind":       renamed.Register(&testRule{id: "ok", kind: "cost"}),
		"duplicate":      renamed.Register(&testRule{id: "bucket-public-access", kind: KindSecurity}),
		"alias taken":    renamed.Register(&testRule{id: "public-bucket", kind: KindSecurity}),
		"alias unknown":  renamed.Alias("old", "missing"),
		"alias twice":    renamed.Alias("other-old", "bucket-public-access"),
		"alias of alias": renamed.Alias("public-bucket", "bucket-public-access"),
	} {
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if !errors.Is(renamed.Alias("old", "missing"), ErrUnknownRule) {
		t.Fatal("alias to an unknown rule")
	}
}
