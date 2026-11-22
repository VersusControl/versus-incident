package collect

import (
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// A policy document is never stored. Each allow statement is reduced to who
// it names (an account, a role or user ARN, an organization, or anyone),
// what class of action it allows, and which part of the resource it covers.
// Conditions are read only for the organization and account they narrow a
// wildcard principal to; their values (ExternalIds, source ARNs, tags) are
// dropped.

// policyGrant is one allow statement reduced to one principal.
type policyGrant struct {
	principal string
	access    string
	pattern   string
}

type policyDocument struct {
	Statement policyList `json:"Statement"`
}

type policyStatement struct {
	Effect       string                     `json:"Effect"`
	Principal    json.RawMessage            `json:"Principal"`
	NotPrincipal json.RawMessage            `json:"NotPrincipal"`
	Action       stringList                 `json:"Action"`
	NotAction    stringList                 `json:"NotAction"`
	Resource     stringList                 `json:"Resource"`
	NotResource  stringList                 `json:"NotResource"`
	Condition    map[string]json.RawMessage `json:"Condition"`
}

// policyList is a Statement written as one object or as a list.
type policyList []policyStatement

func (list *policyList) UnmarshalJSON(data []byte) error {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) > 0 && data[0] == '{' {
		var one policyStatement
		if err := json.Unmarshal(data, &one); err != nil {
			return err
		}
		*list = policyList{one}
		return nil
	}
	var many []policyStatement
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*list = many
	return nil
}

// stringList is a policy value written as one string or as a list.
type stringList []string

func (list *stringList) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*list = stringList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*list = many
	return nil
}

// parsePolicy decodes a policy document as AWS returns it: plain JSON, or
// percent-encoded the way IAM's query API returns a trust policy.
func parsePolicy(document string) (policyDocument, bool) {
	document = strings.TrimSpace(document)
	if strings.HasPrefix(document, "%") {
		decoded, err := url.QueryUnescape(document)
		if err != nil {
			return policyDocument{}, false
		}
		document = decoded
	}
	var parsed policyDocument
	if document == "" || json.Unmarshal([]byte(document), &parsed) != nil {
		return policyDocument{}, false
	}
	return parsed, true
}

var (
	accountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)
	orgIDPattern     = regexp.MustCompile(`o-[a-z0-9]{10,32}`)
	// uniqueIDPattern is an IAM unique ID. A trust policy shows one in place
	// of a principal that was deleted after the policy named it.
	uniqueIDPattern = regexp.MustCompile(`^(AROA|AIDA|AGPA|ANPA|ANVA|AKIA|ASIA)[A-Z0-9]{12,}$`)
)

// narrowingOperator reports whether a condition operator can only match
// when the key is present and equal to (or like) one of its values: the
// positive string and ARN operators, alone or under ForAnyValue. A negated
// operator, an IfExists operator (true when the key is absent) and
// ForAllValues (true for an empty set) narrow nothing.
func narrowingOperator(operator string) bool {
	operator = strings.ToLower(operator)
	if strings.HasSuffix(operator, "ifexists") {
		return false
	}
	operator = strings.TrimPrefix(operator, "foranyvalue:")
	switch operator {
	case "stringequals", "stringequalsignorecase", "stringlike", "arnequals", "arnlike":
		return true
	}
	return false
}

var (
	exactOrgID   = regexp.MustCompile(`^o-[a-z0-9]{10,32}$`)
	orgPathStart = regexp.MustCompile(`^(o-[a-z0-9]{10,32})/`)
)

// conditionNarrowing reads the organizations or accounts a statement's
// conditions narrow a wildcard principal to. Every value of the key is
// read; a key with any value that names no single organization or account
// (a wildcard, a malformed ID) narrows nothing, so the grant stays open.
func conditionNarrowing(conditions map[string]json.RawMessage) (orgs, accounts []string) {
	for operator, block := range conditions {
		if !narrowingOperator(operator) {
			continue
		}
		var keys map[string]json.RawMessage
		if json.Unmarshal(block, &keys) != nil {
			continue
		}
		for key, raw := range keys {
			var values stringList
			if json.Unmarshal(raw, &values) != nil || len(values) == 0 {
				continue
			}
			var found []string
			switch strings.ToLower(key) {
			case "aws:principalorgid":
				for _, value := range values {
					if !exactOrgID.MatchString(value) {
						found = nil
						break
					}
					found = append(found, value)
				}
				orgs = appendNew(orgs, found...)
			case "aws:principalorgpaths":
				for _, value := range values {
					match := orgPathStart.FindStringSubmatch(value)
					if match == nil {
						found = nil
						break
					}
					found = append(found, match[1])
				}
				orgs = appendNew(orgs, found...)
			case "aws:principalaccount", "kms:calleraccount":
				for _, value := range values {
					if !accountIDPattern.MatchString(value) {
						found = nil
						break
					}
					found = append(found, value)
				}
				accounts = appendNew(accounts, found...)
			}
		}
	}
	sort.Strings(orgs)
	sort.Strings(accounts)
	return orgs, accounts
}

func appendNew(list []string, values ...string) []string {
	for _, value := range values {
		seen := false
		for _, item := range list {
			if item == value {
				seen = true
				break
			}
		}
		if !seen {
			list = append(list, value)
		}
	}
	return list
}

// statementPrincipals lists the principals a statement names, normalized
// (normalizePrincipal). AWS service principals are left out: they are AWS,
// not an account. The wildcard is returned as AnyPrincipal.
func statementPrincipals(raw json.RawMessage) []string {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return nil
	}
	var star string
	if json.Unmarshal(raw, &star) == nil {
		if star == "*" {
			return []string{model.AnyPrincipal}
		}
		return nil
	}
	var byType map[string]stringList
	if json.Unmarshal(raw, &byType) != nil {
		return nil
	}
	var out []string
	for _, kind := range []string{"AWS", "Federated"} {
		for _, value := range byType[kind] {
			principal := normalizePrincipal(value)
			if kind == "Federated" && principal == "" {
				// A web identity provider named by host (accounts.google.com,
				// cognito-identity.amazonaws.com): the provider is kept, never
				// the subject a condition names.
				principal = strings.ToLower(strings.TrimSpace(value))
			}
			if principal != "" {
				out = append(out, principal)
			}
		}
	}
	return out
}

// normalizePrincipal is the form a principal is kept in: an account root
// becomes the bare account ID; an assumed-role session ARN becomes the
// role's ARN, so a session name (often a person's email) is never kept; a
// federated user's ARN becomes its account, for the same reason; role,
// IAM user and provider ARNs stay; a unique ID of a deleted principal
// stays as the ID.
func normalizePrincipal(value string) string {
	value = strings.TrimSpace(value)
	switch {
	case value == "*":
		return model.AnyPrincipal
	case accountIDPattern.MatchString(value):
		return value
	case uniqueIDPattern.MatchString(value):
		return value
	case strings.HasPrefix(value, "arn:"):
		parts := strings.SplitN(value, ":", 6)
		if len(parts) < 6 || !accountIDPattern.MatchString(parts[4]) {
			return ""
		}
		switch {
		case parts[2] == "iam" && parts[5] == "root":
			return parts[4]
		case parts[2] == "sts" && strings.HasPrefix(parts[5], "federated-user/"):
			// A federated user's ARN carries the name it signed in with.
			return parts[4]
		case parts[2] == "sts" && strings.HasPrefix(parts[5], "assumed-role/"):
			role, _, _ := strings.Cut(strings.TrimPrefix(parts[5], "assumed-role/"), "/")
			if role == "" {
				return parts[4]
			}
			return "arn:" + parts[1] + ":iam::" + parts[4] + ":role/" + role
		}
		return value
	}
	return ""
}

// principalAccount is the account a principal belongs to, for a grant that
// keeps the account only. An organization, a deleted principal's ID,
// anyone and a provider named by host stay as they are.
func principalAccount(principal string) string {
	if strings.HasPrefix(principal, "arn:") {
		if parts := strings.SplitN(principal, ":", 6); len(parts) == 6 && accountIDPattern.MatchString(parts[4]) {
			return parts[4]
		}
	}
	return principal
}

// readVerbs start the name of an action that only reads.
var readVerbs = []string{"get", "list", "describe", "head", "batchget", "batchcheck", "lookup", "search", "select",
	"query", "scan", "decrypt", "verify"}

// adminVerbs start the name of an action that can change a policy, a
// grant or an ACL.
var adminVerbs = []string{"put", "set", "create", "delete", "update", "attach", "detach", "add", "remove", "replace",
	"revoke", "retire", "modify", "enable", "disable"}

// changesAccess reports whether an action name (lower case) changes a
// policy, a grant, an ACL or permissions.
func changesAccess(name string) bool {
	return strings.Contains(name, "policy") || strings.Contains(name, "acl") || strings.Contains(name, "grant") ||
		strings.Contains(name, "permission")
}

// accessClass reduces a statement's actions to read, write or admin. A
// read (of data, metadata, a policy, an ACL or the grants) is read. A
// change to a policy, a grant or an ACL is admin, and so is a wildcard that
// covers one: "*", "s3:*", "s3:Put*", "ecr:Set*". Any other change is
// write. NotAction allows everything else, so it is admin.
func accessClass(actions, notActions []string) string {
	if len(notActions) > 0 {
		return model.AccessAdmin
	}
	class := ""
	rank := map[string]int{"": 0, model.AccessRead: 1, model.AccessWrite: 2, model.AccessAdmin: 3}
	for _, action := range actions {
		action = strings.ToLower(strings.TrimSpace(action))
		_, name, _ := strings.Cut(action, ":")
		if action == "*" {
			name = "*"
		}
		prefix, wildcard := strings.CutSuffix(name, "*")
		var this string
		switch {
		case !wildcard && hasAnyPrefix(name, readVerbs...):
			this = model.AccessRead
		case wildcard && prefix != "" && hasAnyPrefix(prefix, readVerbs...) && !coversAdminVerb(prefix):
			this = model.AccessRead
		case wildcard && (coversAdminVerb(prefix) || changesAccess(prefix)):
			this = model.AccessAdmin
		case !wildcard && changesAccess(name):
			this = model.AccessAdmin
		default:
			this = model.AccessWrite
		}
		if rank[this] > rank[class] {
			class = this
		}
	}
	return class
}

// coversAdminVerb reports whether a wildcard prefix can expand to an action
// that starts with an admin verb: the prefix is part of the verb ("se",
// "put", "") or runs past it ("putobject", "createg", "setrepository"),
// where the rest of the name may still be a policy, an ACL or a grant.
func coversAdminVerb(prefix string) bool {
	for _, verb := range adminVerbs {
		if strings.HasPrefix(verb, prefix) || strings.HasPrefix(prefix, verb) {
			return true
		}
	}
	return false
}

func hasAnyPrefix(value string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

// resourcePattern reduces a statement's resources to the part of subject it
// covers: empty for the whole resource (the resource itself, everything in
// it, or "*" in a policy that can only name its own resource), else the
// prefix pattern below the resource ("logs/prod-*"). subjectName is the
// resource part an ARN of subject ends in ("my-bucket").
func resourcePattern(resources, notResources []string, subjectName string) string {
	if len(notResources) > 0 {
		return ""
	}
	patterns := map[string]bool{}
	for _, resource := range resources {
		resource = strings.TrimSpace(resource)
		if resource == "*" {
			return ""
		}
		part := resource
		if strings.HasPrefix(resource, "arn:") {
			if parts := strings.SplitN(resource, ":", 6); len(parts) == 6 {
				part = parts[5]
			}
		}
		if subjectName != "" {
			part = strings.TrimPrefix(part, subjectName)
			part = strings.TrimPrefix(part, "/")
		}
		if part == "" || part == "*" {
			return ""
		}
		patterns[part] = true
	}
	if len(patterns) == 0 {
		return ""
	}
	out := make([]string, 0, len(patterns))
	for pattern := range patterns {
		out = append(out, pattern)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// reducePolicy reduces a resource policy to its allow grants. owner is the
// account that owns the resource: its own root is the default holder of
// every key and bucket and is left out. A wildcard principal narrowed by an
// organization condition becomes the organization; narrowed by an account
// condition it becomes that account, and is left out when that is owner.
func reducePolicy(document, owner, subjectName string, keep func(actions []string) bool) []policyGrant {
	parsed, ok := parsePolicy(document)
	if !ok {
		return nil
	}
	var out []policyGrant
	for _, statement := range parsed.Statement {
		if !strings.EqualFold(statement.Effect, "Allow") || len(statement.NotPrincipal) > 0 {
			continue
		}
		if keep != nil && !keep(statement.Action) {
			continue
		}
		orgs, accounts := conditionNarrowing(statement.Condition)
		access := accessClass(statement.Action, statement.NotAction)
		pattern := resourcePattern(statement.Resource, statement.NotResource, subjectName)
		for _, principal := range statementPrincipals(statement.Principal) {
			narrowed := []string{principal}
			if principal == model.AnyPrincipal {
				switch {
				case len(orgs) > 0:
					narrowed = narrowed[:0]
					for _, org := range orgs {
						narrowed = append(narrowed, model.OrgTarget(org))
					}
				case len(accounts) > 0:
					narrowed = accounts
				}
			}
			for _, each := range narrowed {
				if each == owner {
					continue
				}
				out = append(out, policyGrant{principal: each, access: access, pattern: pattern})
			}
		}
	}
	return out
}

// assumeActions keeps the statements of a trust policy that let a principal
// assume the role.
func assumeActions(actions []string) bool {
	for _, action := range actions {
		lower := strings.ToLower(action)
		if lower == "*" || lower == "sts:*" || strings.HasPrefix(lower, "sts:assumerole") {
			return true
		}
	}
	return false
}

// grantRelations turns grants into relations of one type and source.
func grantRelations(kind model.RelationType, source string, grants []policyGrant) []model.Relation {
	out := make([]model.Relation, 0, len(grants))
	for _, grant := range grants {
		relation := model.Relation{Type: kind, Target: grant.principal, Kind: model.RelationKindMayAccess, Source: source,
			Access: grant.access, Pattern: grant.pattern}
		switch kind {
		case model.RelationTrusts:
			// A trust keeps the role it trusts.
			relation.Access, relation.Pattern = "", ""
		case model.RelationMayAccess:
			// A resource policy grant keeps the account it lets in, never
			// the role or user inside it.
			relation.Target = principalAccount(grant.principal)
		}
		out = append(out, relation)
	}
	return out
}
