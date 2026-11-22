package model

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// declaredGapReasons scans model.go for every GapReason constant, so a new
// reason cannot ship without a decided outcome.
func declaredGapReasons(t *testing.T) []GapReason {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "model.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []GapReason
	for _, decl := range file.Decls {
		general, ok := decl.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, spec := range general.Specs {
			value := spec.(*ast.ValueSpec)
			if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != "GapReason" {
				continue
			}
			for _, expr := range value.Values {
				literal, ok := expr.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Fatalf("a GapReason constant is not a string literal: %#v", expr)
				}
				reason, _ := strconv.Unquote(literal.Value)
				out = append(out, GapReason(reason))
			}
		}
	}
	return out
}

// Every declared gap reason has exactly one decided outcome, and the
// outcomes split cleanly into gaps (not read, capped) and answers that
// leave nothing missing (off in AWS, not offered).
func TestEveryDeclaredGapReasonHasOneDecidedOutcome(t *testing.T) {
	decided := map[GapReason]ReadOutcome{
		"denied":                  OutcomeNotRead,
		"throttled":               OutcomeNotRead,
		"not_on_plan":             OutcomeNotRead,
		"not_checked":             OutcomeNotRead,
		"capped":                  OutcomeCapped,
		"not_enabled":             OutcomeOffInAWS,
		"not_available_in_region": OutcomeNotOffered,
	}
	declared := declaredGapReasons(t)
	if len(declared) != len(decided) {
		t.Fatalf("model.go declares %d gap reasons %v; the outcome table decides %d", len(declared), declared, len(decided))
	}
	perOutcome := map[ReadOutcome]int{}
	for _, reason := range declared {
		want, ok := decided[reason]
		if !ok {
			t.Errorf("gap reason %q has no decided outcome", reason)
			continue
		}
		got := OutcomeForReason(reason)
		if got != want || (Gap{Reason: reason}).Outcome() != want {
			t.Errorf("reason %q = %s, want %s", reason, got, want)
		}
		perOutcome[got]++
	}
	for _, outcome := range []ReadOutcome{OutcomeNotRead, OutcomeCapped, OutcomeOffInAWS, OutcomeNotOffered} {
		if perOutcome[outcome] == 0 {
			t.Errorf("no reason maps to %s", outcome)
		}
	}
	gaps := map[ReadOutcome]bool{OutcomeNotRead: true, OutcomeCapped: true, OutcomeOffInAWS: false, OutcomeNotOffered: false, "other": false}
	for outcome, want := range gaps {
		if outcome.IsGap() != want {
			t.Errorf("%q IsGap = %t, want %t", outcome, outcome.IsGap(), want)
		}
	}
}

// An AccessDenied answer is never Off in AWS: only the explicit "not
// enabled" words of the unit vocabulary are.
func TestOnlyNotEnabledWordsReadAsOffInAWS(t *testing.T) {
	for _, code := range []string{"denied", "trust_denied", "external_id_mismatch", "role_not_found", "AccessDenied",
		"AccessDeniedException", "not_on_plan", "throttled", "timeout", "network", "capped", "Not_Enabled", " not_enabled"} {
		if got := (Gap{Reason: GapReasonForCode(code)}).Outcome(); got == OutcomeOffInAWS {
			t.Errorf("code %q reads as off in AWS", code)
		}
	}
	for _, code := range []string{"not_enabled", "region_disabled"} {
		if got := (Gap{Reason: GapReasonForCode(code)}).Outcome(); got != OutcomeOffInAWS {
			t.Errorf("code %q = %s, want off in AWS", code, got)
		}
	}
}
