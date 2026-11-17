package finops

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops/narrative"
)

// Savings kinds.
const (
	SavingIdle         = "idle"
	SavingRightsize    = "rightsize"
	SavingNetworkPath  = "network_path"
	SavingCommitment   = "commitment"
	SavingStorageClass = "storage_class"
)

// Effort and risk vocabularies.
const (
	EffortMinutes = "minutes"
	EffortHours   = "hours"
	EffortDays    = "days"

	RiskNone         = "none"
	RiskLow          = "low"
	RiskNeedsTesting = "needs_testing"
)

// Idle resource kinds the rules understand.
const (
	IdleUnattachedVolume = "unattached_volume"
	IdleEmptyLB          = "empty_load_balancer"
	IdleStoppedVolumes   = "stopped_instance_volumes"
	IdleOldSnapshot      = "old_snapshot"
	IdleUnusedIP         = "unused_ip"
	IdleIdleInstance     = "idle_instance"
)

// Savings bases: where a saving's monthly amount comes from.
const (
	// BasisBilled is computed from billed cost lines.
	BasisBilled = "billed"
	// BasisProvider is the provider's own recommendation estimate.
	BasisProvider = "provider"
	// BasisListPrice is the provider's public list price.
	BasisListPrice = "list_price"
)

// Savings gap reasons: a check that could not run is named, never zero.
const (
	GapNotCollected   = "not_collected"
	GapProviderOff    = "provider_unavailable"
	GapNoUsageMetrics = "no_usage_metrics"
	// GapNeedsUsageDetail: the check needs usage-type detail the cost
	// source does not read for this account (an Organization payer keeps
	// account × service lines).
	GapNeedsUsageDetail = "needs_usage_detail"
)

// SavingsGap names a savings check the source could not run.
type SavingsGap struct {
	Check  string `json:"check"` // an idle kind, or network_path, storage_class, commitment, rightsize
	Reason string `json:"reason"`
}

// Network path targets: managed services reachable through a private
// endpoint instead of a NAT gateway.
const (
	TargetObjectStorage     = "object_storage"
	TargetContainerRegistry = "container_registry"
	TargetKeyValue          = "key_value"
)

// Guide is a read-only how-to the user runs themselves. Versus never applies
// a saving.
type Guide struct {
	Steps       []string `json:"steps,omitempty"`
	ConsolePath string   `json:"console_path,omitempty"`
	Snippet     string   `json:"snippet,omitempty"` // infrastructure-as-code snippet
	DocURL      string   `json:"doc_url,omitempty"`
}

// IdleResource is an inventory finding.
type IdleResource struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Account string `json:"account"`
	Region  string `json:"region"`
	Team    string `json:"team,omitempty"`
	Monthly Micros `json:"monthly_micros"`
	// Count is how many resources the entry stands for (default 1), when
	// the source knows the count but not each ID.
	Count int    `json:"count,omitempty"`
	Basis string `json:"basis,omitempty"`
}

// Recommendation is a provider rightsizing recommendation.
type Recommendation struct {
	ResourceID string `json:"resource_id"`
	Name       string `json:"name,omitempty"`
	Account    string `json:"account"`
	Region     string `json:"region"`
	Team       string `json:"team,omitempty"`
	Current    string `json:"current"`
	Target     string `json:"target"`
	Monthly    Micros `json:"monthly_micros"`
	Source     string `json:"source"`
	Guide      *Guide `json:"guide,omitempty"`
}

// NetworkPath is one network's NAT processing to managed services.
type NetworkPath struct {
	Network string `json:"network"` // VPC or equivalent
	Account string `json:"account"`
	Region  string `json:"region"`
	Team    string `json:"team,omitempty"`
	// Processing is the monthly NAT processing cost per target.
	Processing map[string]Micros `json:"processing_micros"`
	// HasEndpoint names targets that already have a private endpoint.
	HasEndpoint map[string]bool `json:"has_endpoint"`
	// EndpointCost is the monthly cost of adding an endpoint per target
	// (zero for free gateway endpoints).
	EndpointCost map[string]Micros `json:"endpoint_cost_micros"`
	// TargetNames are the provider's names for targets ("S3").
	TargetNames map[string]string `json:"target_names,omitempty"`
	// UpTo marks Processing as the network's whole NAT processing: the part
	// bound for these targets is not known, so the saving is at most that.
	UpTo bool `json:"up_to,omitempty"`
	// Endpoints is how many endpoints the targets need, when the targets
	// are combined under one key.
	Endpoints int    `json:"endpoints,omitempty"`
	Guide     *Guide `json:"guide,omitempty"`
}

// Coverage is commitment (savings plan / reserved) coverage.
type Coverage struct {
	CoveredBP int64 `json:"covered_bp"`
	// Suggestion is the provider's or adapter's purchase suggestion that
	// closes the gap; EstimatedMonthly its saving.
	Suggestion       string `json:"suggestion,omitempty"`
	EstimatedMonthly Micros `json:"estimated_monthly_micros"`
	Guide            *Guide `json:"guide,omitempty"`
}

// Bucket is object storage metadata (no object reads).
type Bucket struct {
	Name             string `json:"name"`
	Account          string `json:"account"`
	Region           string `json:"region"`
	Team             string `json:"team,omitempty"`
	HasLifecycle     bool   `json:"has_lifecycle"`
	StandardBytes    int64  `json:"standard_bytes"`
	EstimatedMonthly Micros `json:"estimated_monthly_micros"`
	Guide            *Guide `json:"guide,omitempty"`
}

// SavingsInputs are the neutral facts the savings rules evaluate.
type SavingsInputs struct {
	Idle            []IdleResource   `json:"idle,omitempty"`
	Recommendations []Recommendation `json:"recommendations,omitempty"`
	NetworkPaths    []NetworkPath    `json:"network_paths,omitempty"`
	Coverage        *Coverage        `json:"coverage,omitempty"`
	Buckets         []Bucket         `json:"buckets,omitempty"`
	// IdleGuides holds a guide per idle kind.
	IdleGuides map[string]*Guide `json:"idle_guides,omitempty"`
	// Gaps name the checks that could not run.
	Gaps []SavingsGap `json:"gaps,omitempty"`
}

// SavingsOptions tune the rules.
type SavingsOptions struct {
	TargetCoverageBP int64 // default 8000 (80%)
	// MinStorageBytes: buckets smaller than this are not worth a lifecycle
	// rule (default 1 TiB).
	MinStorageBytes int64
	// MinMonthly drops items saving less than this (default $1).
	MinMonthly Micros
}

func (o SavingsOptions) withDefaults() SavingsOptions {
	if o.TargetCoverageBP <= 0 {
		o.TargetCoverageBP = 8_000
	}
	if o.MinStorageBytes <= 0 {
		o.MinStorageBytes = 1 << 40
	}
	if o.MinMonthly <= 0 {
		o.MinMonthly = MicrosPerUnit
	}
	return o
}

// Saving is one savings item.
type Saving struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Title       string   `json:"title"`
	Monthly     Micros   `json:"monthly_micros"`
	Effort      string   `json:"effort"`
	Risk        string   `json:"risk"`
	Account     string   `json:"account,omitempty"`
	Region      string   `json:"region,omitempty"`
	ResourceIDs []string `json:"resource_ids,omitempty"`
	Team        string   `json:"team,omitempty"`
	Guide       *Guide   `json:"guide,omitempty"`
	// Basis says where Monthly comes from: billed, provider or list_price.
	Basis string `json:"basis,omitempty"`
	// UpTo marks Monthly as an upper bound.
	UpTo bool `json:"up_to,omitempty"`
	// Count is the number of resources the item covers.
	Count int `json:"count,omitempty"`
	// Alternatives are other actions on the same resource; the item keeps
	// the largest saving and the alternatives are never added to totals.
	Alternatives []SavingAlternative `json:"alternatives,omitempty"`
	// BeforeOtherSavings marks a commitment: its saving is computed on
	// today's usage, before the other savings shrink it, so it is shown on
	// its own and left out of the savings total.
	BeforeOtherSavings bool `json:"before_other_savings,omitempty"`
}

// SavingAlternative is another action on an item's resource.
type SavingAlternative struct {
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Monthly Micros `json:"monthly_micros"`
	Basis   string `json:"basis,omitempty"`
}

var idleTitles = map[string][2]string{ // singular, plural
	IdleUnattachedVolume: {"unattached disk volume", "unattached disk volumes"},
	IdleEmptyLB:          {"load balancer with no targets", "load balancers with no targets"},
	IdleStoppedVolumes:   {"stopped machine still paying for disks", "stopped machines still paying for disks"},
	IdleOldSnapshot:      {"old snapshot", "old snapshots"},
	IdleUnusedIP:         {"unused public IP address", "unused public IP addresses"},
	IdleIdleInstance:     {"idle machine", "idle machines"},
}

var idleRisk = map[string]string{
	IdleUnattachedVolume: RiskLow,
	IdleEmptyLB:          RiskNone,
	IdleStoppedVolumes:   RiskLow,
	IdleOldSnapshot:      RiskLow,
	IdleUnusedIP:         RiskNone,
	IdleIdleInstance:     RiskNeedsTesting,
}

// EvaluateSavings applies the savings rules to neutral inputs. Results are
// sorted by monthly saving, largest first.
func EvaluateSavings(in SavingsInputs, opts SavingsOptions) []Saving {
	opts = opts.withDefaults()
	var out []Saving
	in, alternatives := oneActionPerResource(in)

	// Idle: one item per kind and account.
	type idleGroup struct {
		kind, account, region, team string
		ids                         []string
		count                       int
		monthly                     Micros
		basis                       string
	}
	groups := map[string]*idleGroup{}
	for _, r := range in.Idle {
		if _, ok := idleTitles[r.Kind]; !ok || r.Monthly <= 0 {
			continue
		}
		k := r.Kind + "|" + r.Account
		g := groups[k]
		if g == nil {
			g = &idleGroup{kind: r.Kind, account: r.Account, region: r.Region, team: r.Team}
			groups[k] = g
		}
		if g.region != r.Region {
			g.region = ""
		}
		if g.team != r.Team {
			g.team = ""
		}
		g.ids = append(g.ids, r.ID)
		g.count += max(1, r.Count)
		g.monthly += r.Monthly
		if g.basis == "" || g.basis == r.Basis {
			g.basis = r.Basis
		} else {
			g.basis = BasisListPrice
		}
	}
	for _, k := range sortedKeys(groups) {
		g := groups[k]
		sort.Strings(g.ids)
		t := idleTitles[g.kind]
		noun := t[1]
		if g.count == 1 {
			noun = t[0]
		}
		verb := "Remove"
		if g.kind == IdleIdleInstance {
			verb = "Stop or remove"
		}
		// The ID names the resources as well as the kind and account: an
		// idle saving that comes back with other resources (lb-A dismissed
		// and deleted, lb-B idle now) is a new saving and does not inherit
		// the decision made on the first one.
		out = append(out, Saving{
			ID: shortHash(append([]string{"saving", SavingIdle, k}, g.ids...)...), Kind: SavingIdle,
			Title:   fmt.Sprintf("%s %d %s", verb, g.count, noun),
			Monthly: g.monthly, Effort: EffortMinutes, Risk: idleRisk[g.kind],
			Account: g.account, Region: g.region, ResourceIDs: g.ids, Team: g.team,
			Guide: in.IdleGuides[g.kind], Basis: g.basis, Count: g.count,
		})
	}

	for _, r := range in.Recommendations {
		if r.Monthly <= 0 || r.Target == "" {
			continue
		}
		name := r.Name
		if name == "" {
			name = r.ResourceID
		}
		out = append(out, Saving{
			ID: shortHash("saving", SavingRightsize, r.Account, r.ResourceID), Kind: SavingRightsize,
			Title:   "Resize " + name + " to " + r.Target,
			Monthly: r.Monthly, Effort: EffortHours, Risk: RiskNeedsTesting,
			Account: r.Account, Region: r.Region, ResourceIDs: []string{r.ResourceID}, Team: r.Team, Guide: r.Guide,
			Basis: BasisProvider,
		})
	}

	for _, n := range in.NetworkPaths {
		var targets []string
		var saving Micros
		for _, t := range sortedKeys(n.Processing) {
			if n.HasEndpoint[t] {
				continue
			}
			if s := n.Processing[t] - n.EndpointCost[t]; s > 0 {
				targets = append(targets, t)
				saving += s
			}
		}
		if len(targets) == 0 {
			continue
		}
		names := make([]string, len(targets))
		for i, t := range targets {
			names[i] = n.TargetNames[t]
			if names[i] == "" {
				names[i] = strings.ReplaceAll(t, "_", " ")
			}
		}
		noun := "private endpoints"
		if max(len(targets), n.Endpoints) == 1 {
			noun = "a private endpoint"
		}
		out = append(out, Saving{
			ID: shortHash("saving", SavingNetworkPath, n.Account, n.Network), Kind: SavingNetworkPath,
			Title:   "Add " + noun + " for " + narrative.JoinList(names) + " in " + n.Network,
			Monthly: saving, Effort: EffortHours, Risk: RiskLow,
			Account: n.Account, Region: n.Region, ResourceIDs: []string{n.Network}, Team: n.Team, Guide: n.Guide,
			Basis: BasisBilled, UpTo: n.UpTo,
		})
	}

	if c := in.Coverage; c != nil && c.CoveredBP < opts.TargetCoverageBP && c.EstimatedMonthly > 0 {
		title := fmt.Sprintf("Raise discount coverage from %d%% toward %d%%", c.CoveredBP/100, opts.TargetCoverageBP/100)
		if sug := PlainCommitmentSuggestion("", c.Suggestion); sug != "" {
			title += ": " + sug
		}
		out = append(out, Saving{
			ID: shortHash("saving", SavingCommitment), Kind: SavingCommitment, Title: title,
			Monthly: c.EstimatedMonthly, Effort: EffortHours, Risk: RiskLow, Guide: c.Guide, Basis: BasisProvider,
			BeforeOtherSavings: true,
		})
	}

	for _, b := range in.Buckets {
		if b.HasLifecycle || b.StandardBytes < opts.MinStorageBytes || b.EstimatedMonthly <= 0 {
			continue
		}
		out = append(out, Saving{
			ID: shortHash("saving", SavingStorageClass, b.Account, b.Name), Kind: SavingStorageClass,
			Title:   "Add a lifecycle rule to " + b.Name + " to move cold data to a cheaper class",
			Monthly: b.EstimatedMonthly, Effort: EffortMinutes, Risk: RiskLow,
			Account: b.Account, Region: b.Region, ResourceIDs: []string{b.Name}, Team: b.Team, Guide: b.Guide,
		})
	}

	kept := out[:0]
	for _, s := range out {
		if s.Monthly < opts.MinMonthly {
			continue
		}
		for _, id := range s.ResourceIDs {
			s.Alternatives = append(s.Alternatives, alternatives[s.Account+"|"+id]...)
		}
		kept = append(kept, s)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].Monthly != kept[j].Monthly {
			return kept[i].Monthly > kept[j].Monthly
		}
		return kept[i].ID < kept[j].ID
	})
	return kept
}

// SavingState is the stored decision on a savings item. Account, Kind and
// Title are what the item was when the decision was made, kept with the
// decision so it still names the item after the item stops being produced;
// they are not part of the API.
type SavingState struct {
	Dismissed bool      `json:"dismissed"`
	Reason    string    `json:"reason,omitempty"`
	By        string    `json:"by,omitempty"`
	At        time.Time `json:"at,omitzero"`
	Account   string    `json:"-"`
	Kind      string    `json:"-"`
	Title     string    `json:"-"`
}

// savingOption is one candidate action on a resource: an idle entry or a
// recommendation, by index.
type savingOption struct {
	monthly Micros
	idle    int // index into Idle, or -1
	rec     int // index into Recommendations, or -1
}

// oneActionPerResource keeps, for each account and resource, the action
// with the largest saving (an idle removal or a rightsize) and returns the
// others as alternatives keyed by account and resource, so the same
// resource is never counted twice.
func oneActionPerResource(in SavingsInputs) (SavingsInputs, map[string][]SavingAlternative) {
	best := map[string]savingOption{}
	alts := map[string][]SavingAlternative{}
	consider := func(key string, o savingOption) {
		cur, ok := best[key]
		switch {
		case !ok:
			best[key] = o
		case o.monthly > cur.monthly:
			alts[key] = append(alts[key], alternativeOf(in, cur))
			best[key] = o
		default:
			alts[key] = append(alts[key], alternativeOf(in, o))
		}
	}
	single := func(r IdleResource) bool { return r.ID != "" && max(1, r.Count) == 1 }
	for i, r := range in.Idle {
		if single(r) {
			consider(r.Account+"|"+r.ID, savingOption{r.Monthly, i, -1})
		}
	}
	for i, r := range in.Recommendations {
		if r.ResourceID != "" {
			consider(r.Account+"|"+r.ResourceID, savingOption{r.Monthly, -1, i})
		}
	}
	keepIdle, keepRec := map[int]bool{}, map[int]bool{}
	for _, o := range best {
		if o.idle >= 0 {
			keepIdle[o.idle] = true
		} else {
			keepRec[o.rec] = true
		}
	}
	out := in
	out.Idle, out.Recommendations = nil, nil
	for i, r := range in.Idle {
		if !single(r) || keepIdle[i] {
			out.Idle = append(out.Idle, r)
		}
	}
	for i, r := range in.Recommendations {
		if r.ResourceID == "" || keepRec[i] {
			out.Recommendations = append(out.Recommendations, r)
		}
	}
	for k := range alts {
		sort.SliceStable(alts[k], func(i, j int) bool { return alts[k][i].Monthly > alts[k][j].Monthly })
	}
	return out, alts
}

func alternativeOf(in SavingsInputs, o savingOption) SavingAlternative {
	if o.idle >= 0 {
		r := in.Idle[o.idle]
		verb := "Remove"
		if r.Kind == IdleIdleInstance {
			verb = "Stop or remove"
		}
		return SavingAlternative{Kind: SavingIdle, Title: verb + " " + idleTitles[r.Kind][0] + " " + r.ID, Monthly: r.Monthly, Basis: r.Basis}
	}
	r := in.Recommendations[o.rec]
	name := r.Name
	if name == "" {
		name = r.ResourceID
	}
	return SavingAlternative{Kind: SavingRightsize, Title: "Resize " + name + " to " + r.Target, Monthly: r.Monthly, Basis: BasisProvider}
}

var (
	plainSuggestion  = regexp.MustCompile(`^a(?: [0-9]+-year)?(?: [a-z][a-z ]*)? commitment of \$[0-9]+(?:\.[0-9]+)?/h$`)
	suggestionHourly = regexp.MustCompile(`\$\s*([0-9]+(?:\.[0-9]+)?)\s*/\s*h(?:ou)?r\b`)
	suggestionTerm   = regexp.MustCompile(`(?i)\b(one|three|1|3)[ -]years?\b`)
)

// PlainCommitmentSuggestion rewords a provider's commitment purchase summary
// ("$0.626/hour with three years term (AllUpfront) in AWS account … (Linked
// account level)") as a short phrase ("a 3-year compute commitment of
// $0.626/h"). Only the hourly commitment and the term are read; the rest
// (payment option, account, scope) is never echoed. kind names the plan
// ("compute"; "" for none). A phrase already in this form is returned as it
// is, so stored text can be cleaned again on every read. When the hourly
// amount cannot be read the answer is "" and the item's title stands alone.
func PlainCommitmentSuggestion(kind, text string) string {
	text = strings.TrimSpace(text)
	if plainSuggestion.MatchString(text) {
		return text
	}
	m := suggestionHourly.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	s := "a"
	if t := suggestionTerm.FindStringSubmatch(text); t != nil {
		switch strings.ToLower(t[1]) {
		case "one", "1":
			s += " 1-year"
		case "three", "3":
			s += " 3-year"
		}
	}
	if kind != "" {
		s += " " + kind
	}
	return s + " commitment of $" + m[1] + "/h"
}
