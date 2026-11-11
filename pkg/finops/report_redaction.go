package finops

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// redactedTeamLabel replaces a tag-derived team name in a report read while
// the team tag is not allowlisted.
const redactedTeamLabel = "(team hidden)"

// redactedResourceID stands in for a stored resource ID that is a provider
// ARN: it may carry the account ID, so it is never shown as it is.
const redactedResourceID = "(resource ID hidden)"

// readFrozen answers a frozen report for this request. A team tag or Name
// tag the deployment no longer allowlists hides its values in the answer
// while the stored document, and every number in it, stays as it was frozen.
func (s *Service) readFrozen(fr ReportView, sc *scope) (ReportView, error) {
	out, err := plainReportSuggestions(fr)
	if err != nil {
		return ReportView{}, err
	}
	if out, err = redactReportResourceIDs(out); err != nil {
		return ReportView{}, err
	}
	if !sc.st.TeamTag.Allowlisted {
		if out, err = redactReportTeams(out); err != nil {
			return ReportView{}, err
		}
	}
	if !sc.st.NameTagAllowlisted {
		return redactReportNames(out)
	}
	return out, nil
}

// plainReportSuggestions rewords, on read, a commitment suggestion a report
// was frozen with in the provider's own text (which can carry the account
// ID): the same rule as a live read (PlainCommitmentSuggestion), applied to
// every string of a copy. A suggestion that cannot be reworded is dropped
// with its colon. The stored document is not rewritten.
func plainReportSuggestions(fr ReportView) (ReportView, error) {
	with := map[string]string{}
	for _, it := range fr.Savings.Top {
		if it.Kind != SavingCommitment {
			continue
		}
		_, raw, ok := strings.Cut(it.Title, ": ")
		if !ok {
			continue
		}
		if plain := PlainCommitmentSuggestion("", raw); plain != raw {
			with[": "+raw] = ""
			if plain != "" {
				with[": "+raw] = ": " + plain
			}
		}
	}
	if len(with) == 0 {
		return fr, nil
	}
	out, err := copyReportView(fr)
	if err != nil {
		return ReportView{}, err
	}
	scrubStrings(reflect.ValueOf(&out), func(s string) string {
		for raw, plain := range with {
			s = strings.ReplaceAll(s, raw, plain)
		}
		return s
	})
	return out, nil
}

// arnToken finds a provider ARN inside a sentence: "arn:", a partition and
// the rest of the token, without a trailing full stop.
var arnToken = regexp.MustCompile(`(?i)\barn:[a-z0-9-]+:[^\s,;()<>"']*[^\s,;()<>"'.]`)

// redactReportResourceIDs returns a copy of a frozen report in which no
// resource ID is a provider ARN. A report frozen before resource IDs were
// shown through displayResourceID may hold an ARN, account ID included, in
// an item's resource IDs and in the titles and lines that name the resource.
// It runs on every read, whatever the Name tag allowlist says, and scrubs
// every string of the copy; the stored document is not rewritten.
func redactReportResourceIDs(fr ReportView) (ReportView, error) {
	ids := map[string]string{}
	for _, it := range fr.Savings.Top {
		for _, id := range it.ResourceIDs {
			if shown := displayResourceID(id); shown != id {
				ids[id] = shown
			}
		}
	}
	raw, err := json.Marshal(fr)
	if err != nil {
		return ReportView{}, err
	}
	if len(ids) == 0 && !arnToken.Match(raw) {
		return fr, nil
	}
	out, err := copyReportView(fr)
	if err != nil {
		return ReportView{}, err
	}
	scrubStrings(reflect.ValueOf(&out), func(s string) string {
		if shown, ok := ids[s]; ok {
			return shown
		}
		for id, shown := range ids {
			s = strings.ReplaceAll(s, strings.TrimSpace(id), shown)
		}
		return arnToken.ReplaceAllString(s, redactedResourceID)
	})
	return out, nil
}

// redactReportNames returns a copy of a frozen report whose rightsize items
// name their resource by ID instead of by the Name tag value the report was
// frozen with ("Resize reporting to m6i.medium" → "Resize i-0abc to
// m6i.medium"). Like the team rule it runs on read only and scrubs every
// string of the copy, so the name is also gone from the item's line and from
// any sentence that quotes the saving.
func redactReportNames(fr ReportView) (ReportView, error) {
	names := map[string]string{}
	for _, it := range fr.Savings.Top {
		if it.Kind != SavingRightsize || len(it.ResourceIDs) == 0 {
			continue
		}
		id := displayResourceID(it.ResourceIDs[0])
		for _, title := range append([]string{it.Title}, alternativeTitles(it.Alternatives)...) {
			if name := rightsizeName(title); name != "" && name != it.ResourceIDs[0] && name != id {
				names[name] = id
			}
		}
	}
	if len(names) == 0 {
		return fr, nil
	}
	out, err := copyReportView(fr)
	if err != nil {
		return ReportView{}, err
	}
	scrubStrings(reflect.ValueOf(&out), func(s string) string { return replaceWords(s, names) })
	return out, nil
}

func alternativeTitles(alts []SavingAlternative) []string {
	var out []string
	for _, a := range alts {
		if a.Kind == SavingRightsize {
			out = append(out, a.Title)
		}
	}
	return out
}

// rightsizeName is the resource name in a rightsize title ("Resize <name> to
// <target>"); "" when the title has another shape.
func rightsizeName(title string) string {
	rest, ok := strings.CutPrefix(title, "Resize ")
	if !ok {
		return ""
	}
	i := strings.LastIndex(rest, " to ")
	if i <= 0 {
		return ""
	}
	return rest[:i]
}

// displayResourceID is a stored resource ID fit to show: an ID that is or
// looks like a provider ARN is replaced, never shown raw.
func displayResourceID(id string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(id)), "arn:") {
		return redactedResourceID
	}
	return id
}

// replaceWords replaces each whole-word key of with by its value, longer
// keys first so a name containing another is replaced in one piece.
func replaceWords(s string, with map[string]string) string {
	if s == "" {
		return s
	}
	ordered := make([]string, 0, len(with))
	for k := range with {
		ordered = append(ordered, k)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i]) != len(ordered[j]) {
			return len(ordered[i]) > len(ordered[j])
		}
		return ordered[i] < ordered[j]
	})
	for _, k := range ordered {
		s = replaceWord(s, k, with[k])
	}
	return s
}

// redactReportTeams returns a copy of a frozen report with tag-derived team
// labels removed. It runs on read only: the document in storage keeps its
// labels and every one of its numbers, so a freeze still means the figures
// never change and widening the allowlist again restores the labels. Nothing
// is written back.
//
// The labels are collected from the document itself — the team split,
// tag-owned rows, savings items and scope choices — and then every string
// field of the copy is scrubbed. A team field added to the model later is
// therefore redacted too, instead of leaking because this helper did not know
// its name.
func redactReportTeams(fr ReportView) (ReportView, error) {
	out, err := copyReportView(fr)
	if err != nil {
		return ReportView{}, err
	}
	labels := reportTeamLabels(out)
	if len(labels) == 0 {
		return out, nil
	}
	scrubStrings(reflect.ValueOf(&out), func(s string) string { return scrubTeamText(s, labels) })
	return out, nil
}

// copyReportView deep-copies a report through its own JSON shape. A store may
// hand back slices shared with the document it retains, and the scrub must
// never reach storage.
func copyReportView(fr ReportView) (ReportView, error) {
	raw, err := json.Marshal(fr)
	if err != nil {
		return ReportView{}, err
	}
	var out ReportView
	if err := json.Unmarshal(raw, &out); err != nil {
		return ReportView{}, err
	}
	return out, nil
}

// reportTeamLabels collects every tag-derived team name a report carries:
// the spend-by-team keys and labels, the tag-owned owner of any row, saving
// or expected increase, the saving team, and the scope bar's team choices.
func reportTeamLabels(r ReportView) map[string]bool {
	labels := map[string]bool{}
	add := func(name string) {
		if name != "" && name != Unallocated {
			labels[name] = true
		}
	}
	addOwner := func(o *Owner) {
		if o != nil && o.Rule == "tag" {
			add(o.Name)
		}
	}
	for _, row := range r.SpendByTeam {
		add(row.Key)
		add(row.Label)
		addOwner(row.Owner)
	}
	for _, row := range r.Bridge.Rows {
		addOwner(row.Owner)
	}
	if r.Bridge.Other != nil {
		addOwner(r.Bridge.Other.Owner)
	}
	for _, it := range r.Savings.Top {
		add(it.Team)
		addOwner(&it.Owner)
	}
	for _, in := range r.Notes.Expected {
		addOwner(&in.Owner)
	}
	for _, t := range r.Summary.ScopeOptions.Teams {
		add(t)
	}
	return labels
}

// scrubStrings rewrites every string field of v with scrub, descending
// through structs, pointers, interfaces and slices.
func scrubStrings(v reflect.Value, scrub func(string) string) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			scrubStrings(v.Elem(), scrub)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); f.CanSet() {
				scrubStrings(f, scrub)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			scrubStrings(v.Index(i), scrub)
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(scrub(v.String()))
		}
	}
}

// scrubTeamText replaces whole-word team labels in one string. Longer labels
// go first so a label containing another is replaced in one piece.
func scrubTeamText(s string, labels map[string]bool) string {
	if s == "" {
		return s
	}
	ordered := make([]string, 0, len(labels))
	for l := range labels {
		ordered = append(ordered, l)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i]) != len(ordered[j]) {
			return len(ordered[i]) > len(ordered[j])
		}
		return ordered[i] < ordered[j]
	})
	for _, l := range ordered {
		s = replaceWord(s, l, redactedTeamLabel)
		// A sentence starts with the owner's name capitalized.
		if c := titleFirst(l); c != l {
			s = replaceWord(s, c, redactedTeamLabel)
		}
	}
	return s
}

// titleFirst upper-cases the first rune, matching how a sentence names a team.
func titleFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return s
	}
	up := unicode.ToUpper(r)
	if up == r {
		return s
	}
	return string(up) + s[size:]
}

// replaceWord replaces label where it stands as its own word: bounded by the
// string ends or by a character that is not a letter, digit or underscore. A
// one-letter team name therefore cannot rewrite every word of a sentence.
func replaceWord(s, label, with string) string {
	if label == "" {
		return s
	}
	var b strings.Builder
	i := 0
	for {
		j := strings.Index(s[i:], label)
		if j < 0 {
			b.WriteString(s[i:])
			return b.String()
		}
		j += i
		end := j + len(label)
		if !wordRuneBefore(s, j) && !wordRuneAt(s, end) {
			b.WriteString(s[i:j])
			b.WriteString(with)
			i = end
			continue
		}
		b.WriteString(s[i:end])
		i = end
	}
}

// wordRuneBefore reports whether the rune ending just before i is a word rune.
func wordRuneBefore(s string, i int) bool {
	if i <= 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return isWordRune(r)
}

// wordRuneAt reports whether the rune starting at i is a word rune.
func wordRuneAt(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return isWordRune(r)
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
