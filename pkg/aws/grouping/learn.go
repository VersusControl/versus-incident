package grouping

import (
	"sort"
	"strings"
	"unicode"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// Thresholds a tag key must meet in one account to be learned as an
// application key.
const (
	// MinTagShare is the least share of the account's resources, in
	// percent, that must carry a usable value for the key.
	MinTagShare = 10
	// MinTagValues and MaxTagValues bound how many distinct values the key
	// has: one value says nothing about applications, hundreds are IDs.
	MinTagValues = 2
	MaxTagValues = 200
)

// TagKeys are the application tag keys learned per account, best first.
// Each key is a family: every spelling of one idea ("App", "app",
// "application_name") reads as the same key, so its groups keep one ID.
type TagKeys map[string][]string

// knownFamilies are tried before any other key, in this order.
var knownFamilies = []string{"app", "service", "app.kubernetes.io/name", "kubernetes.io/service-name"}

// familySynonyms fold spellings of the same idea onto one family.
var familySynonyms = map[string]string{
	"application": "app", "apps": "app", "applicationid": "app",
	"svc": "service", "services": "service",
}

// controllerKeyPrefixes start tag keys AWS or a Kubernetes controller
// writes. They describe ownership, not an application.
var controllerKeyPrefixes = []string{
	"aws:", "kubernetes.io/", "k8s.io/", "karpenter.sh/", "karpenter.k8s.aws/", "eks:", "elbv2.k8s.aws/",
	"service.k8s.aws/", "ingress.k8s.aws/", "ebs.csi.aws.com/", "efs.csi.aws.com/", "alpha.eksctl.io/", "sigs.k8s.io/",
}

// notAppFamilies are keys that are never an application: the display name
// and the provenance markers tools write.
var notAppFamilies = map[string]bool{
	"name": true, "managedby": true, "createdby": true, "terraform": true, "version": true,
	"description": true, "repository": true, "repo": true, "source": true,
}

// deniedWords are the words of environment, owner and team keys. A key
// holding one as a separate part ("BusinessOwner", "app_env", "AppTeam")
// is denied; "envoy" is not.
var deniedWords = map[string]bool{"env": true, "environment": true, "stage": true, "owner": true, "team": true}

// deniedStems deny a key family that starts or ends with them, written as
// one word ("owneremail", "businessowner", "costcenter").
var deniedStems = []string{"owner", "team", "environment", "stage", "cost"}

// envQualifiers are the words that follow "env" in a one-word environment
// key ("envtype", "envid"); "envoy" is not one.
var envQualifiers = map[string]bool{"type": true, "id": true, "name": true, "tier": true, "class": true, "kind": true, "code": true, "group": true}

// keyWords are a tag key's word parts, lower case. Words break at
// separators, at a lower-to-upper case change, and before the last capital
// of an acronym that a capitalised word follows: "BusinessOwner" →
// business, owner; "DBOwner" → db, owner; "service-cost-center" → service,
// cost, center.
func keyWords(key string) []string {
	var words []string
	var current []rune
	runes := []rune(key)
	flush := func() {
		if len(current) > 0 {
			words = append(words, strings.ToLower(string(current)))
			current = current[:0]
		}
	}
	for index, r := range runes {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
			continue
		case unicode.IsUpper(r) && len(current) > 0:
			previous := runes[index-1]
			nextLower := index+1 < len(runes) && unicode.IsLower(runes[index+1])
			if unicode.IsLower(previous) || unicode.IsDigit(previous) || (unicode.IsUpper(previous) && nextLower) {
				flush()
			}
		}
		current = append(current, r)
	}
	flush()
	return words
}

// deniedKey reports an environment, owner, team, stage or cost key in any
// spelling: a family that starts or ends with one of those words, or ends
// with env, or is env with a qualifier ("envtype"); or a key with one of
// them as a separate word part.
func deniedKey(key, family string) bool {
	for _, stem := range deniedStems {
		if strings.HasPrefix(family, stem) || strings.HasSuffix(family, stem) {
			return true
		}
	}
	if strings.HasSuffix(family, "env") || (strings.HasPrefix(family, "env") && envQualifiers[family[len("env"):]]) {
		return true
	}
	for _, word := range keyWords(key) {
		if deniedWords[word] || strings.HasPrefix(word, "cost") {
			return true
		}
	}
	return false
}

// Family is the form a tag key is compared in: lower case, without
// separators, a trailing "name" and synonyms folded. A key with a path or a
// domain (app.kubernetes.io/name) is only lower-cased.
func Family(key string) string {
	family := strings.ToLower(strings.TrimSpace(key))
	if strings.ContainsAny(family, "/.") {
		return family
	}
	family = strings.NewReplacer("-", "", "_", "", " ", "", ":", "").Replace(family)
	if len(family) > len("name") && strings.HasSuffix(family, "name") {
		family = strings.TrimSuffix(family, "name")
	}
	if synonym, ok := familySynonyms[family]; ok {
		family = synonym
	}
	return family
}

// appFamily reports whether a key may be learned as an application key.
func appFamily(key, family string) bool {
	known := false
	for _, candidate := range knownFamilies {
		if family == candidate {
			known = true
		}
	}
	if known {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(key))
	for _, prefix := range controllerKeyPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	return family != "" && !notAppFamilies[family] && !strings.HasPrefix(family, "git") && !deniedKey(key, family)
}

// usableValue is a tag value grouping may use: set, and not hidden by the
// tag-value allowlist.
func usableValue(value string) (string, bool) {
	value = strings.TrimSpace(value)
	return value, value != "" && value != model.RedactedValue
}

// familyValue is the resource's value for a family: the first usable value
// among its spellings, in key order so the answer does not depend on map
// order.
func familyValue(resource model.Resource, family string) (string, bool) {
	var keys []string
	for key := range resource.Tags {
		if Family(key) == family {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if value, ok := usableValue(resource.Tags[key]); ok {
			return value, true
		}
	}
	return "", false
}

// Learn finds each account's application tag keys. A key family is learned
// when at least MinTagShare percent of the account's resources carry a
// usable value for it, it has MinTagValues to MaxTagValues distinct values
// (case folded), and it is not an environment, owner, cost, name or
// controller key. The known spellings come first, in their order; any
// other learned key follows, the most common first.
func Learn(resources []model.Resource) TagKeys {
	type stats struct {
		covered int
		values  map[string]bool
	}
	totals := map[string]int{}
	families := map[string]map[string]*stats{}
	for _, resource := range resources {
		if !groupable(resource) {
			continue
		}
		totals[resource.Account]++
		byFamily := families[resource.Account]
		if byFamily == nil {
			byFamily = map[string]*stats{}
			families[resource.Account] = byFamily
		}
		seen := map[string]bool{}
		for key := range resource.Tags {
			family := Family(key)
			if seen[family] || !appFamily(key, family) {
				continue
			}
			value, ok := familyValue(resource, family)
			if !ok {
				continue
			}
			seen[family] = true
			entry := byFamily[family]
			if entry == nil {
				entry = &stats{values: map[string]bool{}}
				byFamily[family] = entry
			}
			entry.covered++
			if len(entry.values) <= MaxTagValues {
				entry.values[strings.ToLower(value)] = true
			}
		}
	}
	out := TagKeys{}
	for account, byFamily := range families {
		qualifies := func(family string) bool {
			entry := byFamily[family]
			return entry != nil && entry.covered*100 >= MinTagShare*totals[account] &&
				len(entry.values) >= MinTagValues && len(entry.values) <= MaxTagValues
		}
		var learned, others []string
		known := map[string]bool{}
		for _, family := range knownFamilies {
			known[family] = true
			if qualifies(family) {
				learned = append(learned, family)
			}
		}
		for family := range byFamily {
			if !known[family] && qualifies(family) {
				others = append(others, family)
			}
		}
		sort.Slice(others, func(left, right int) bool {
			a, b := byFamily[others[left]].covered, byFamily[others[right]].covered
			if a != b {
				return a > b
			}
			return others[left] < others[right]
		})
		if learned = append(learned, others...); len(learned) > 0 {
			out[account] = learned
		}
	}
	return out
}
