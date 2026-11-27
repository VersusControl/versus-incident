package issues

import (
	"errors"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// Read is how the latest read of one input ended for an account.
type Read string

// Read outcomes. Only ReadOK and ReadNotOffered leave nothing missing.
const (
	ReadOK         Read = "ok"
	ReadNotRead    Read = "not_read"
	ReadCapped     Read = "capped"
	ReadOffInAWS   Read = "off_in_aws"
	ReadNotOffered Read = "not_offered"
)

// ReadFor maps a gap's read outcome onto a Read.
func ReadFor(outcome model.ReadOutcome) Read {
	switch outcome {
	case model.OutcomeCapped:
		return ReadCapped
	case model.OutcomeOffInAWS:
		return ReadOffInAWS
	case model.OutcomeNotOffered:
		return ReadNotOffered
	}
	return ReadNotRead
}

// Combine folds the reads of one input across an account's Regions. A
// Region not read or capped leaves the account's read missing; otherwise
// one Region read is a read, since a feature off in one Region or a
// service not offered there leaves nothing missing. Only an input off in
// every Region it was tried in is off in AWS.
func Combine(a, b Read) Read {
	rank := func(read Read) int {
		switch read {
		case ReadNotOffered:
			return 0
		case ReadOffInAWS:
			return 1
		case ReadOK:
			return 2
		case ReadCapped:
			return 3
		}
		return 4
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// Input is one source a rule reads: a collector unit service.
type Input struct {
	Service string `json:"service"`
}

// ViewData is what a View is built from.
type ViewData struct {
	Account   string
	AsOf      time.Time
	Resources []model.Resource
	Health    []model.Health
	Gaps      []model.Gap
	// Reads is the latest read of each input service in each Region. A
	// unit with no entry was never read.
	Reads map[ReadKey]Read
}

// ReadKey names one unit's read: a service in a Region.
type ReadKey struct {
	Service string
	Region  string
}

// View is a read-only snapshot of one account that rules evaluate.
type View struct{ data ViewData }

// NewView builds a view; it copies what it is given.
func NewView(data ViewData) View {
	data.Resources = slices.Clone(data.Resources)
	data.Health = slices.Clone(data.Health)
	data.Gaps = slices.Clone(data.Gaps)
	reads := make(map[ReadKey]Read, len(data.Reads))
	for key, read := range data.Reads {
		reads[key] = read
	}
	data.Reads = reads
	return View{data: data}
}

// Account is the account the view describes.
func (view View) Account() string { return view.data.Account }

// AsOf is when the view's data was read.
func (view View) AsOf() time.Time { return view.data.AsOf }

// Resources returns a copy of the account's resources.
func (view View) Resources() []model.Resource { return slices.Clone(view.data.Resources) }

// Health returns a copy of the account's health items.
func (view View) Health() []model.Health { return slices.Clone(view.data.Health) }

// Gaps returns a copy of the account's gaps.
func (view View) Gaps() []model.Gap { return slices.Clone(view.data.Gaps) }

// Read is the latest read of one input service across the account's
// Regions, for an account-wide rule; never read is not read.
func (view View) Read(service string) Read {
	var read Read
	for key, one := range view.data.Reads {
		if key.Service != service {
			continue
		}
		if read == "" {
			read = one
			continue
		}
		read = Combine(read, one)
	}
	if read == "" {
		return ReadNotRead
	}
	return read
}

// ReadIn is the latest read of one input service in one Region. A service
// read only globally (IAM, the S3 bucket list) answers with its global
// read; a gap in another Region never counts. Never read is not read.
func (view View) ReadIn(service, region string) Read {
	if region == "" {
		return view.Read(service)
	}
	if read, ok := view.data.Reads[ReadKey{Service: service, Region: region}]; ok {
		return read
	}
	if read, ok := view.data.Reads[ReadKey{Service: service, Region: model.GlobalRegion}]; ok {
		return read
	}
	return ReadNotRead
}

// Regions lists the Regions the view has reads for, sorted, without the
// global pseudo-Region.
func (view View) Regions() []string {
	seen := map[string]bool{}
	var out []string
	for key := range view.data.Reads {
		if key.Region != "" && key.Region != model.GlobalRegion && !seen[key.Region] {
			seen[key.Region] = true
			out = append(out, key.Region)
		}
	}
	sort.Strings(out)
	return out
}

// Finding is one problem a rule found on one owner.
type Finding struct {
	// Owner is the stable owner: an ARN, or a key such as "root" that the
	// issue ID scopes to the account. Never an instance that churns.
	Owner    string
	Region   string
	GroupID  string
	Severity Severity
	// Tier is optional; empty uses TierFor(Severity).
	Tier     Tier
	Title    string
	Evidence []Evidence
	Steps    []Step
	// Since is when the problem started, or zero when unknown.
	Since time.Time
	// Material is what a rejection's digest covers. Volatile facts (a
	// last-used time, a count) stay out of it.
	Material     Material
	HygieneCount int
}

// Rule finds one kind of problem. Evaluate is a pure function over the
// view: it never calls AWS and never writes.
type Rule interface {
	ID() string
	Kind() Kind
	Inputs() []Input
	Evaluate(View) []Finding
}

// ruleIDPattern is a rule ID: lowercase, stable, stored with every issue.
var ruleIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,127}$`)

// Registry errors.
var (
	ErrInvalidRuleID = errors.New("issues: invalid rule id")
	ErrDuplicateRule = errors.New("issues: rule id already registered")
	ErrUnknownRule   = errors.New("issues: unknown rule id")
)

// Registry holds the rules the evaluator runs. A rule ID is stored with
// every issue and hashed into its ID, so it never changes: a rule that is
// renamed keeps its old ID through Alias, and its issues, rejections
// included, keep theirs.
type Registry struct {
	mu      sync.RWMutex
	rules   map[string]Rule
	aliases map[string]string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{rules: map[string]Rule{}, aliases: map[string]string{}}
}

// Default is the registry the evaluator runs.
var Default = NewRegistry()

// Register adds a rule.
func (registry *Registry) Register(rule Rule) error {
	id := rule.ID()
	if !ruleIDPattern.MatchString(id) || (rule.Kind() != KindReliability && rule.Kind() != KindSecurity) {
		return ErrInvalidRuleID
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, ok := registry.rules[id]; ok {
		return ErrDuplicateRule
	}
	if _, ok := registry.aliases[id]; ok {
		return ErrDuplicateRule
	}
	registry.rules[id] = rule
	return nil
}

// Alias records that the rule once registered as old is now current, so
// issues raised under old keep their IDs. current is a registered ID or an
// older alias in the same line; renaming twice chains the aliases.
func (registry *Registry) Alias(old, current string) error {
	if !ruleIDPattern.MatchString(old) {
		return ErrInvalidRuleID
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	_, registered := registry.rules[current]
	_, renamed := registry.aliases[current]
	if !registered && !renamed {
		return ErrUnknownRule
	}
	if _, ok := registry.rules[old]; ok {
		return ErrDuplicateRule
	}
	if _, ok := registry.aliases[old]; ok {
		return ErrDuplicateRule
	}
	for _, next := range registry.aliases {
		// One rule keeps one line of old IDs; two old IDs on one rule
		// would leave its issues two IDs.
		if next == current {
			return ErrDuplicateRule
		}
	}
	registry.aliases[old] = current
	return nil
}

// Rules returns the registered rules in ID order.
func (registry *Registry) Rules() []Rule {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	out := make([]Rule, 0, len(registry.rules))
	for _, rule := range registry.rules {
		out = append(out, rule)
	}
	sort.Slice(out, func(left, right int) bool { return out[left].ID() < out[right].ID() })
	return out
}

// IDs returns every rule ID an issue may carry: the registered IDs and the
// old IDs kept by Alias, sorted.
func (registry *Registry) IDs() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	out := make([]string, 0, len(registry.rules)+len(registry.aliases))
	for id := range registry.rules {
		out = append(out, id)
	}
	for id := range registry.aliases {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Lookup returns the rule for id, following aliases.
func (registry *Registry) Lookup(id string) (Rule, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	for range len(registry.aliases) + 1 {
		if rule, ok := registry.rules[id]; ok {
			return rule, true
		}
		next, ok := registry.aliases[id]
		if !ok {
			return nil, false
		}
		id = next
	}
	return nil, false
}

// StableID is the ID a rule's issues are keyed on: the oldest ID the rule
// was known by.
func (registry *Registry) StableID(current string) string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	for range len(registry.aliases) {
		found := false
		for old, next := range registry.aliases {
			if next == current {
				current, found = old, true
				break
			}
		}
		if !found {
			break
		}
	}
	return current
}

// names is every ID the issues of the rule current may carry: current and
// its old IDs.
func (registry *Registry) names(current string) []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	out := []string{current}
	for range len(registry.aliases) {
		found := false
		for old, next := range registry.aliases {
			if next == out[len(out)-1] {
				out, found = append(out, old), true
				break
			}
		}
		if !found {
			break
		}
	}
	return out
}
