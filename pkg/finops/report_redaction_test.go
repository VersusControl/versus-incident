package finops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// reportTeamStrings is every field of a report the security gate names as
// team-labelled, plus the scope team choices and the copy sentence.
func reportTeamStrings(r ReportView) []string {
	var out []string
	for _, row := range r.SpendByTeam {
		out = append(out, row.Key, row.Label)
		if row.Owner != nil {
			out = append(out, row.Owner.Name, row.Owner.RuleText)
		}
	}
	for _, in := range r.Notes.Expected {
		out = append(out, in.Owner.Name, in.Owner.RuleText, in.Sentence)
	}
	for _, it := range r.Savings.Top {
		out = append(out, it.Team, it.Owner.Name, it.Line)
	}
	out = append(out, r.Summary.ScopeOptions.Teams...)
	out = append(out, r.Bridge.OwnerColumn.Note, r.CopySummary)
	return out
}

func containsTeamLabel(r ReportView, label string) bool {
	for _, s := range reportTeamStrings(r) {
		if strings.Contains(strings.ToLower(s), strings.ToLower(label)) {
			return true
		}
	}
	return false
}

// TestFrozenReportRedactsTeamLabelsOnRead proves a frozen report never shows
// a tag value the deployment no longer allowlists, without touching the
// frozen document: the numbers stay identical, narrowing the allowlist hides
// the labels on read, and widening it brings them back.
func TestFrozenReportRedactsTeamLabelsOnRead(t *testing.T) {
	src, cfg := natStory()
	st := newMemStore()
	svc := NewService(src, st, cfg).WithSuspectFinder(storySuspects())
	ctx := t.Context()
	if _, err := svc.SyncIncreases(ctx); err != nil {
		t.Fatal(err)
	}
	incs, err := svc.Increases(ctx, Query{Period: "2026-10", Status: "all"})
	if err != nil {
		t.Fatal(err)
	}
	var natID string
	for _, it := range incs.Items {
		if it.Service == "network.nat" {
			natID = it.ID
		}
	}
	if natID == "" {
		t.Fatal("the story's NAT increase must exist")
	}
	if _, err := svc.SetIncreaseStatus(ctx, natID, StatusChange{Status: StatusExpected, Reason: "release", Until: MustDate("2026-11-20")}, "admin"); err != nil {
		t.Fatal(err)
	}

	frozen, err := svc.FreezeReport(ctx, "2026-10", "admin")
	if err != nil || frozen.Frozen == nil {
		t.Fatalf("freeze: %+v %v", frozen.Frozen, err)
	}
	if len(frozen.SpendByTeam) == 0 || len(frozen.Notes.Expected) == 0 || len(frozen.Savings.Top) == 0 {
		t.Fatalf("the story must fill the team-labelled fields: %d %d %d",
			len(frozen.SpendByTeam), len(frozen.Notes.Expected), len(frozen.Savings.Top))
	}
	for _, label := range []string{"checkout", "platform"} {
		if !containsTeamLabel(frozen, label) {
			t.Fatalf("the allowlisted report must name team %q", label)
		}
	}
	frozenJSON, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}

	// Narrow the allowlist: no write, and the read hides every label.
	src.status.TeamTag.Allowlisted = false
	narrowed := NewService(src, st, cfg).WithSuspectFinder(storySuspects())
	redacted, err := narrowed.Report(ctx, october)
	if err != nil || redacted.Frozen == nil {
		t.Fatalf("frozen read: %+v %v", redacted.Frozen, err)
	}
	for _, label := range []string{"checkout", "platform"} {
		if containsTeamLabel(redacted, label) {
			t.Fatalf("the narrowed report still names team %q", label)
		}
	}
	redactedJSON, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(redactedJSON), []byte("checkout")) {
		t.Fatalf("a tag value survives somewhere in the narrowed report: %s", redactedJSON)
	}
	assertReportNumbersEqual(t, frozen, redacted)
	// The narrowed read reports the same freeze, so it is the frozen copy.
	if redacted.Frozen.At != frozen.Frozen.At || redacted.Frozen.By != frozen.Frozen.By {
		t.Fatalf("frozen marker changed: %+v vs %+v", redacted.Frozen, frozen.Frozen)
	}

	// The stored document was never rewritten: widening restores it exactly.
	src.status.TeamTag.Allowlisted = true
	widened := NewService(src, st, cfg).WithSuspectFinder(storySuspects())
	back, err := widened.Report(ctx, october)
	if err != nil {
		t.Fatal(err)
	}
	backJSON, err := json.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backJSON, frozenJSON) {
		t.Fatal("the frozen document changed under a narrowed allowlist")
	}
	if !containsTeamLabel(back, "checkout") {
		t.Fatal("widening the allowlist must bring the label back")
	}

	// Freezing again answers the same frozen copy, redacted under the narrow
	// allowlist, and never rewrites it.
	src.status.TeamTag.Allowlisted = false
	again, err := NewService(src, st, cfg).FreezeReport(ctx, "2026-10", "admin")
	if err != ErrAlreadyFrozen || containsTeamLabel(again, "checkout") {
		t.Fatalf("second freeze: %v, labels present %v", err, containsTeamLabel(again, "checkout"))
	}
}

// assertReportNumbersEqual proves only labels changed: the counts, totals and
// every spend-by-team amount still match the frozen document.
func assertReportNumbersEqual(t *testing.T, frozen, got ReportView) {
	t.Helper()
	if got.Summary.Total != frozen.Summary.Total || got.Bridge.Total != frozen.Bridge.Total ||
		got.Bridge.Base != frozen.Bridge.Base || got.Bridge.Change != frozen.Bridge.Change ||
		got.Savings.Total.Monthly != frozen.Savings.Total.Monthly || got.Savings.Total.Count != frozen.Savings.Total.Count ||
		len(got.SpendByTeam) != len(frozen.SpendByTeam) || len(got.Notes.Expected) != len(frozen.Notes.Expected) ||
		len(got.Savings.Top) != len(frozen.Savings.Top) {
		t.Fatalf("labels changed a number: %+v vs %+v", got.Summary, frozen.Summary)
	}
	for i := range frozen.SpendByTeam {
		if got.SpendByTeam[i].Amount != frozen.SpendByTeam[i].Amount ||
			got.SpendByTeam[i].ShareBP != frozen.SpendByTeam[i].ShareBP {
			t.Fatalf("spend-by-team row %d changed: %+v vs %+v", i, got.SpendByTeam[i], frozen.SpendByTeam[i])
		}
	}
	for i := range frozen.Notes.Expected {
		if got.Notes.Expected[i].Monthly != frozen.Notes.Expected[i].Monthly ||
			got.Notes.Expected[i].DailyBefore != frozen.Notes.Expected[i].DailyBefore {
			t.Fatalf("expected increase %d changed: %+v vs %+v", i, got.Notes.Expected[i], frozen.Notes.Expected[i])
		}
	}
	for i := range frozen.Savings.Top {
		if got.Savings.Top[i].Monthly != frozen.Savings.Top[i].Monthly {
			t.Fatalf("savings item %d changed: %+v vs %+v", i, got.Savings.Top[i], frozen.Savings.Top[i])
		}
	}
}

// TestReportTeamLabelsCoverEveryTeamField walks the ReportView type graph and
// proves reportTeamLabels reads every string field the model labels as a team
// or owner (by Go name or JSON tag). A new team-labelled field therefore
// fails here until the collector learns the path it lives on.
func TestReportTeamLabelsCoverEveryTeamField(t *testing.T) {
	var doc ReportView
	populateTeamDoc(reflect.ValueOf(&doc), map[reflect.Type]bool{})
	markOwnerRules(reflect.ValueOf(&doc), map[reflect.Type]bool{})
	var fields []teamField
	markTeamFields(reflect.ValueOf(&doc), "ReportView", &fields, map[reflect.Type]bool{})
	if len(fields) == 0 {
		t.Fatal("the walk found no team-labelled string field; the guard itself is stale")
	}
	got := reportTeamLabels(doc)
	for _, f := range fields {
		if !got[f.sentinel] {
			t.Fatalf("team-labelled field %s is not read by reportTeamLabels: teach the collector that path", f.path)
		}
	}
	redacted, err := redactReportTeams(doc)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fields {
		if bytes.Contains(raw, []byte(f.sentinel)) {
			t.Fatalf("team-labelled field %s survived the scrub: the walk and the scrub disagree", f.path)
		}
	}
}

// populateTeamDoc gives every slice one element and every pointer struct a
// value, so a field added anywhere in the graph is reachable by the walk.
func populateTeamDoc(v reflect.Value, seen map[reflect.Type]bool) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		populateTeamDoc(v.Elem(), seen)
	case reflect.Struct:
		// BreakdownRow nests itself through Inside; stop at the first level.
		if seen[v.Type()] {
			return
		}
		seen[v.Type()] = true
		defer delete(seen, v.Type())
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); f.CanSet() {
				populateTeamDoc(f, seen)
			}
		}
	case reflect.Slice:
		switch v.Type().Elem().Kind() {
		case reflect.Struct, reflect.String:
			if v.IsNil() {
				v.Set(reflect.MakeSlice(v.Type(), 1, 1))
			}
			for i := 0; i < v.Len(); i++ {
				populateTeamDoc(v.Index(i), seen)
			}
		}
	}
}

// markOwnerRules sets every owner's rule to the tag rule, the only rule that
// makes the collector treat an owner name as a team value.
func markOwnerRules(v reflect.Value, seen map[reflect.Type]bool) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			markOwnerRules(v.Elem(), seen)
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(Owner{}) {
			v.FieldByName("Rule").SetString("tag")
			return
		}
		if seen[v.Type()] {
			return
		}
		seen[v.Type()] = true
		defer delete(seen, v.Type())
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); f.CanSet() {
				markOwnerRules(f, seen)
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			markOwnerRules(v.Index(i), seen)
		}
	}
}

// teamField is one team-labelled string field and the sentinel written there.
type teamField struct {
	path     string
	sentinel string
}

// markTeamFields writes a distinct sentinel into every string field (or
// string slice element) whose Go name or JSON tag mentions team or owner.
func markTeamFields(v reflect.Value, path string, fields *[]teamField, seen map[reflect.Type]bool) {
	t := v.Type()
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			markTeamFields(v.Elem(), path, fields, seen)
		}
	case reflect.Struct:
		if seen[t] {
			return
		}
		seen[t] = true
		defer delete(seen, t)
		for i := 0; i < t.NumField(); i++ {
			f, sf := v.Field(i), t.Field(i)
			if !f.CanSet() {
				continue
			}
			tag := strings.ToLower(strings.Split(sf.Tag.Get("json"), ",")[0])
			name := strings.ToLower(sf.Name)
			labelled := strings.Contains(name, "team") || strings.Contains(name, "owner") ||
				strings.Contains(tag, "team") || strings.Contains(tag, "owner")
			field := path + "." + sf.Name
			switch {
			case labelled && f.Kind() == reflect.String:
				s := fmt.Sprintf("sentinel-team-%d", len(*fields)+1)
				f.SetString(s)
				*fields = append(*fields, teamField{path: field, sentinel: s})
			case labelled && f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.String:
				for j := 0; j < f.Len(); j++ {
					s := fmt.Sprintf("sentinel-team-%d", len(*fields)+1)
					f.Index(j).SetString(s)
					*fields = append(*fields, teamField{path: fmt.Sprintf("%s[%d]", field, j), sentinel: s})
				}
			default:
				markTeamFields(f, field, fields, seen)
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			markTeamFields(v.Index(i), fmt.Sprintf("%s[%d]", path, i), fields, seen)
		}
	}
}

// A frozen report names a rightsized resource by the Name tag value it was
// frozen with. Once Name is no longer allowlisted, a read names it by its
// resource ID everywhere (title, line, alternatives) without rewriting the
// stored document; allowlisting Name again brings the name back.
func TestFrozenReportRetitlesRightsizeItemsWithoutTheNameTag(t *testing.T) {
	src, cfg := natStory()
	src.status.NameTagAllowlisted = true
	src.savings.Recommendations = append(src.savings.Recommendations,
		Recommendation{ResourceID: "i-0abc", Name: "reporting", Account: acctProd, Region: "us-east-1",
			Current: "m6i.large", Target: "m6i.medium", Monthly: usd(5_000), Source: "compute_optimizer"},
		Recommendation{ResourceID: "i-0abc", Name: "reporting", Account: acctProd, Region: "us-east-1",
			Current: "m6i.large", Target: "m6i.small", Monthly: usd(4_000), Source: "cost_optimization_hub"})
	st := newMemStore()
	ctx := t.Context()
	frozen, err := NewService(src, st, cfg).FreezeReport(ctx, "2026-10", "admin")
	if err != nil || frozen.Frozen == nil {
		t.Fatalf("freeze: %v", err)
	}
	var item *SavingView
	for i := range frozen.Savings.Top {
		if frozen.Savings.Top[i].Kind == SavingRightsize {
			item = &frozen.Savings.Top[i]
		}
	}
	if item == nil || item.Title != "Resize reporting to m6i.medium" || len(item.Alternatives) != 1 {
		t.Fatalf("frozen rightsize item: %+v", item)
	}
	frozenJSON, _ := json.Marshal(frozen)

	src.status.NameTagAllowlisted = false
	narrowed, err := NewService(src, st, cfg).Report(ctx, october)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(narrowed)
	if strings.Contains(string(raw), "Resize reporting") || strings.Contains(string(raw), "resizing reporting") {
		t.Fatalf("the Name tag value survives in the narrowed report")
	}
	for _, it := range narrowed.Savings.Top {
		if it.Kind != SavingRightsize {
			continue
		}
		if it.Title != "Resize i-0abc to m6i.medium" || !strings.HasPrefix(it.Line, "Resize i-0abc to m6i.medium:") ||
			it.Alternatives[0].Title != "Resize i-0abc to m6i.small" {
			t.Fatalf("narrowed item: %q / %q / %q", it.Title, it.Line, it.Alternatives[0].Title)
		}
	}
	assertReportNumbersEqual(t, frozen, narrowed)

	src.status.NameTagAllowlisted = true
	back, err := NewService(src, st, cfg).Report(ctx, october)
	if err != nil {
		t.Fatal(err)
	}
	if backJSON, _ := json.Marshal(back); !bytes.Equal(backJSON, frozenJSON) {
		t.Fatal("the frozen document changed under a narrowed allowlist")
	}
}

// A stored resource ID that is or looks like an ARN is never shown raw.
func TestDisplayResourceIDHidesARNs(t *testing.T) {
	for in, want := range map[string]string{
		"i-0abc": "i-0abc",
		"arn:aws:ec2:us-east-1:111122223333:instance/i-0abc": redactedResourceID,
		" ARN:aws:lambda:us-east-1:111122223333:function:x":  redactedResourceID,
	} {
		if got := displayResourceID(in); got != want {
			t.Errorf("displayResourceID(%q) = %q, want %q", in, got, want)
		}
	}
}

// A report frozen with a provider's raw commitment sentence (account ID
// included) reads with the suggestion reworded, without rewriting the
// stored document.
func TestFrozenReportRewordsARawCommitmentSuggestion(t *testing.T) {
	src, cfg := natStory()
	st := newMemStore()
	ctx := t.Context()
	if _, err := NewService(src, st, cfg).FreezeReport(ctx, "2026-10", "admin"); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetReport(ctx, "2026-10")
	if err != nil || stored == nil {
		t.Fatalf("stored report: %v", err)
	}
	const raw = "$0.626/hour with three years term (AllUpfront) in AWS account 111122223333 (Linked account level)"
	n := 0
	for i := range stored.Savings.Top {
		it := &stored.Savings.Top[i]
		if it.Kind != SavingCommitment {
			continue
		}
		head, clean, _ := strings.Cut(it.Title, ": ")
		it.Title = head + ": " + raw
		it.Line = strings.Replace(it.Line, clean, raw, 1)
		n++
	}
	if n == 0 {
		t.Fatal("the story must freeze a commitment item")
	}
	if err := st.PutReport(ctx, *stored); err != nil {
		t.Fatal(err)
	}
	read, err := NewService(src, st, cfg).Report(ctx, october)
	if err != nil {
		t.Fatal(err)
	}
	raw2, _ := json.Marshal(read)
	if strings.Contains(string(raw2), "111122223333") || strings.Contains(string(raw2), "AllUpfront") {
		t.Fatal("the provider's sentence survives in the frozen read")
	}
	for _, it := range read.Savings.Top {
		if it.Kind == SavingCommitment && !strings.HasSuffix(it.Title, ": a 3-year commitment of $0.626/h") {
			t.Fatalf("commitment title = %q", it.Title)
		}
	}
	again, err := st.GetReport(ctx, "2026-10")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range again.Savings.Top {
		if it.Kind == SavingCommitment && !strings.Contains(it.Title, raw) {
			t.Fatal("the stored document was rewritten")
		}
	}
}

// A report frozen while a rightsize item still carried its ARN, account ID
// included, in its resource IDs and title reads with the ARN hidden, with
// the Name tag allowlisted or not, and the stored document keeps it.
func TestFrozenReportHidesARNResourceIDsOnEveryRead(t *testing.T) {
	const arn = "arn:aws:ec2:us-east-1:111122223333:instance/i-0abc"
	src, cfg := natStory()
	src.status.NameTagAllowlisted = true
	src.savings.Recommendations = append(src.savings.Recommendations,
		Recommendation{ResourceID: "i-0abc", Account: acctProd, Region: "us-east-1",
			Current: "m6i.large", Target: "m6i.medium", Monthly: usd(5_000), Source: "compute_optimizer"})
	st := newMemStore()
	ctx := t.Context()
	frozen, err := NewService(src, st, cfg).FreezeReport(ctx, "2026-10", "admin")
	if err != nil || frozen.Frozen == nil {
		t.Fatalf("freeze: %v", err)
	}
	// Rewrite the stored document the way an older build froze it: the raw
	// ARN as the resource ID and in every sentence that names the resource.
	stored := st.reports["2026-10"]
	found := false
	for i := range stored.Savings.Top {
		it := &stored.Savings.Top[i]
		if it.Kind != SavingRightsize {
			continue
		}
		found = true
		it.ResourceIDs = []string{arn}
		it.Title = strings.ReplaceAll(it.Title, "i-0abc", arn)
		it.Line = strings.ReplaceAll(it.Line, "i-0abc", arn)
	}
	if !found {
		t.Fatal("the story must freeze a rightsize item")
	}
	stored.CopySummary += " See " + arn + "."
	st.reports["2026-10"] = stored
	storedJSON, _ := json.Marshal(stored)

	for _, allowlisted := range []bool{true, false} {
		src.status.NameTagAllowlisted = allowlisted
		got, err := NewService(src, st, cfg).Report(ctx, october)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(got)
		if strings.Contains(string(raw), "arn:") || strings.Contains(string(raw), "111122223333") {
			t.Fatalf("Name allowlisted %v: the frozen ARN survives the read", allowlisted)
		}
		for _, it := range got.Savings.Top {
			if it.Kind != SavingRightsize {
				continue
			}
			if len(it.ResourceIDs) != 1 || it.ResourceIDs[0] != redactedResourceID ||
				it.Title != "Resize "+redactedResourceID+" to m6i.medium" ||
				!strings.HasPrefix(it.Line, "Resize "+redactedResourceID+" to m6i.medium") {
				t.Fatalf("Name allowlisted %v: item %q %q %q", allowlisted, it.ResourceIDs, it.Title, it.Line)
			}
		}
		if !strings.HasSuffix(got.CopySummary, " See "+redactedResourceID+".") {
			t.Fatalf("Name allowlisted %v: copy summary %q", allowlisted, got.CopySummary)
		}
		assertReportNumbersEqual(t, stored, got)
	}
	if after, _ := json.Marshal(st.reports["2026-10"]); !bytes.Equal(after, storedJSON) {
		t.Fatal("reading the report rewrote the stored document")
	}
}
