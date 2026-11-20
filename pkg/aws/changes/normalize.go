package changes

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// Event is one LookupEvents record as the collector received it. Raw is the
// CloudTrailEvent document; Normalize reads the identity, the error code and
// the template's identifier fields from it and keeps nothing else.
type Event struct {
	ID          string
	Name        string
	Source      string
	Time        time.Time
	Resources   []string
	ReadOnly    string
	AccessKeyID string
	Raw         string
}

// maxRawBytes bounds the document Normalize is willing to parse.
const maxRawBytes = 256 * 1024

// maxFieldBytes bounds one kept identifier.
const maxFieldBytes = 256

// identifierShape is the character set an allowlisted field may contain.
// Identifiers, ARNs, names, revisions and CIDR blocks fit; free text does not.
var identifierShape = regexp.MustCompile(`^[A-Za-z0-9:/._\-+=,@*]+$`)

type rawEvent struct {
	EventVersion      string          `json:"eventVersion"`
	UserIdentity      Identity        `json:"userIdentity"`
	UserAgent         string          `json:"userAgent"`
	ErrorCode         string          `json:"errorCode"`
	ReadOnly          *bool           `json:"readOnly"`
	RequestParameters json.RawMessage `json:"requestParameters"`
	ResponseElements  json.RawMessage `json:"responseElements"`
}

// signInSources are services whose every event is a sign-in, a token or a
// session: none of them changes a resource.
var signInSources = map[string]bool{
	"signin.amazonaws.com":    true,
	"sts.amazonaws.com":       true,
	"sso.amazonaws.com":       true,
	"sso-oauth.amazonaws.com": true,
}

// cognitoSignIns are the Cognito user pool and identity pool calls that
// sign a user in or hand out tokens or credentials.
var cognitoSignIns = map[string]bool{
	"Token_POST":                  true,
	"InitiateAuth":                true,
	"AdminInitiateAuth":           true,
	"RespondToAuthChallenge":      true,
	"AdminRespondToAuthChallenge": true,
	"GetId":                       true,
	"GetCredentialsForIdentity":   true,
}

// readPrefixes start the names of calls that read. CloudTrail flags some
// of them as writes (readOnly false); they are still reads.
var readPrefixes = []string{"Describe", "List", "Get", "BatchGet", "Lookup", "Search", "Head", "Select"}

// IsNotChange reports an event CloudTrail may flag as a write that changes
// nothing: a sign-in, an authentication or token call, or a read-shaped
// call (Describe…, List…, Get…). Such events are neither stored nor
// counted as changes.
func IsNotChange(source, name string) bool {
	if signInSources[source] {
		return true
	}
	if (source == "cognito-idp.amazonaws.com" || source == "cognito-identity.amazonaws.com") && cognitoSignIns[name] {
		return true
	}
	for _, prefix := range readPrefixes {
		rest, found := strings.CutPrefix(name, prefix)
		if !found {
			continue
		}
		// The prefix must be a whole word: Listen… or Headroom… is not a read.
		if rest == "" || rest[0] >= 'A' && rest[0] <= 'Z' || rest[0] == '_' {
			return true
		}
	}
	return false
}

// Normalize maps one event onto a change. It returns false for read-only
// events and for events IsNotChange reports. The raw document, request parameters and response elements are
// dropped; only the template's identifier fields survive, each checked
// against identifierShape.
func Normalize(account, region string, event Event) (model.Change, bool) {
	if strings.EqualFold(event.ReadOnly, "true") || IsNotChange(event.Source, event.Name) {
		return model.Change{}, false
	}
	var raw rawEvent
	if len(event.Raw) > 0 && len(event.Raw) <= maxRawBytes {
		_ = json.Unmarshal([]byte(event.Raw), &raw)
	}
	if raw.ReadOnly != nil && *raw.ReadOnly {
		return model.Change{}, false
	}

	change := model.Change{
		ID:          event.ID,
		Time:        event.Time.UTC(),
		Account:     account,
		Region:      region,
		Service:     ServiceForSource(event.Source),
		EventSource: event.Source,
		EventName:   event.Name,
		Actor:       ClassifyActor(raw.UserIdentity, raw.UserAgent),
		ErrorCode:   safeIdentifier(raw.ErrorCode),
	}

	// Resources are canonical ARNs, built from the resource type the
	// template knows plus the event's account and Region. A name that
	// cannot be resolved is kept as unresolved, never matched as an ARN.
	scope := arnScope{partition: partitionFor(raw.UserIdentity.ARN, region), region: region, account: account}
	resolved := map[string]bool{}
	addResource := func(arn string) {
		if !contains(change.Resources, arn) {
			change.Resources = append(change.Resources, arn)
		}
	}
	addUnresolved := func(name string) {
		if !resolved[name] && !contains(change.Unresolved, name) {
			change.Unresolved = append(change.Unresolved, name)
		}
	}
	template, ok := Lookup(event.Source, event.Name)
	if ok {
		fields := extractFields(template.Fields, template.Tags, raw.RequestParameters, raw.ResponseElements)
		change.Template = template.ID
		change.Summary = template.Render(fields)
		if len(fields) > 0 {
			change.Fields = fields
		}
		var pending []string
		for _, name := range template.ResourceFields {
			value := fields[name]
			if value == "" {
				continue
			}
			if arn, found := resolveField(event.Source, name, fields, scope); found {
				addResource(arn)
				resolved[value] = true
			} else {
				pending = append(pending, value)
			}
		}
		// A field that only failed because a better field of the same
		// resource resolved (a service name next to its ARN) is not unknown.
		if len(change.Resources) == 0 {
			for _, value := range pending {
				addUnresolved(value)
			}
		}
	} else {
		change.Template = GenericTemplateID
		change.Summary = GenericSummary(event.Source, event.Name)
	}
	for _, name := range event.Resources {
		value := safeIdentifier(name)
		if value == "" || resolved[value] {
			continue
		}
		if arn, found := resolveName(event.Source, value, scope); found {
			addResource(arn)
			resolved[value] = true
			continue
		}
		if !matchesResolved(value, change.Resources) {
			addUnresolved(value)
		}
	}
	if change.ErrorCode != "" {
		change.Summary = "Failed attempt: " + change.Summary
	}
	maskPersonal(&change)
	return change, true
}

var (
	// emailToken finds an email address inside a longer value, such as an
	// IAM user named after a person in a summary or an ARN.
	emailToken = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// accessKeyToken finds an access key ID.
	accessKeyToken = regexp.MustCompile(`(?:AKIA|ASIA)[A-Z0-9]{16}`)
)

// HiddenAccessKey replaces an access key ID in a kept value.
const HiddenAccessKey = "(access key hidden)"

// maskPersonal masks email-shaped names in the summary, the field names and
// values and the resource references, the same way the actor is masked, and
// drops access key IDs: none of them is ever stored. Every template's values pass here.
func maskPersonal(change *model.Change) {
	change.Summary = maskText(change.Summary)
	if len(change.Fields) > 0 {
		// Field names are masked too: a tag key is stored as part of one.
		fields := make(map[string]string, len(change.Fields))
		for name, value := range change.Fields {
			fields[maskText(name)] = maskText(value)
		}
		change.Fields = fields
	}
	// A masked ARN no longer names one resource (two people would share
	// it), so it is kept as unresolved and never matched.
	var resources []string
	for _, arn := range change.Resources {
		if masked := maskText(arn); masked != arn {
			change.Unresolved = append(change.Unresolved, masked)
			continue
		}
		resources = append(resources, arn)
	}
	change.Resources = resources
	change.Unresolved = maskList(change.Unresolved)
}

func maskText(value string) string {
	value = emailToken.ReplaceAllString(value, MaskedEmail)
	return accessKeyToken.ReplaceAllString(value, HiddenAccessKey)
}

// maskList masks each reference and drops one that is only an access key.
func maskList(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := values[:0]
	for _, value := range values {
		if accessKeyToken.MatchString(value) && accessKeyToken.FindString(value) == value {
			continue
		}
		if masked := maskText(value); !contains(out, masked) {
			out = append(out, masked)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func extractFields(fields []Field, tags string, request, response json.RawMessage) map[string]string {
	if len(fields) == 0 && tags == "" {
		return nil
	}
	var requestDoc, responseDoc any
	if len(request) > 0 {
		_ = json.Unmarshal(request, &requestDoc)
	}
	if len(response) > 0 {
		_ = json.Unmarshal(response, &responseDoc)
	}
	at := func(path string) (any, bool) {
		root, rest, found := strings.Cut(path, ".")
		if !found {
			return nil, false
		}
		switch root {
		case "requestParameters":
			return walkAny(requestDoc, strings.Split(rest, ".")), true
		case "responseElements":
			return walkAny(responseDoc, strings.Split(rest, ".")), true
		}
		return nil, false
	}
	out := make(map[string]string, len(fields))
	for _, field := range fields {
		value, ok := at(field.Path)
		if !ok {
			continue
		}
		// A secret-shaped field keeps only the fact that it was set; its
		// value never leaves the event.
		if SecretFieldName(field.Name) {
			if present(value) {
				out[field.Name] = SecretSet
			}
			continue
		}
		if text := safeIdentifier(scalar(value)); text != "" {
			out[field.Name] = text
		}
	}
	if tags != "" {
		if value, ok := at(tags); ok {
			for _, key := range tagKeys(value) {
				out[TagFieldPrefix+key] = model.RedactedValue
			}
		}
	}
	return out
}

// TagFieldPrefix names a field that records one tag a call set. Its value
// is never kept at ingest (it is model.RedactedValue), because ingest does
// not have the tag-value allowlist: the change list always shows the tag
// as hidden, allowlisted or not. Compare shows allowlisted tag values from
// snapshots.
const TagFieldPrefix = "tag:"

// SecretSet is the value kept for a secret-shaped field that a call set.
const SecretSet = "set"

// maxTagFields bounds the tag fields one change keeps.
const maxTagFields = 20

// secretFieldShape matches field names whose values may be secrets.
var secretFieldShape = regexp.MustCompile(`(?i)(password|passwd|secret|token|credential|private.?key|access.?key|api.?key|user.?data|environment|^env$|^env_|auth)`)

// SecretFieldName reports a field whose value may be a secret: it is
// stored only as "set" and shown as hidden.
func SecretFieldName(name string) bool { return secretFieldShape.MatchString(name) }

func present(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return typed != ""
	case map[string]any:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	}
	return true
}

// tagKeys reads the keys of a tag list ([{key, value}] in any letter case)
// or a tag map, in order, keeping identifier-shaped keys only.
func tagKeys(value any) []string {
	var keys []string
	add := func(key string) {
		// A tag key becomes part of a field name, so an email or access key
		// ID in it is masked like any kept value.
		if key = maskText(safeIdentifier(key)); key != "" && len(keys) < maxTagFields && !contains(keys, key) {
			keys = append(keys, key)
		}
	}
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if entry, ok := item.(map[string]any); ok {
				for _, name := range []string{"key", "Key"} {
					if key, ok := entry[name].(string); ok {
						add(key)
					}
				}
			}
		}
	case map[string]any:
		names := make([]string, 0, len(typed))
		for key := range typed {
			names = append(names, key)
		}
		sort.Strings(names)
		for _, key := range names {
			add(key)
		}
	}
	return keys
}

// walkAny follows a dotted path; numeric segments index arrays.
func walkAny(document any, path []string) any {
	current := document
	for _, segment := range path {
		switch typed := current.(type) {
		case map[string]any:
			current = typed[segment]
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(typed) {
				return nil
			}
			current = typed[index]
		default:
			return nil
		}
	}
	return current
}

// scalar returns a scalar as text. Objects and arrays are not identifiers
// and yield nothing.
func scalar(current any) string {
	switch typed := current.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	default:
		return ""
	}
}

func safeIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxFieldBytes || !identifierShape.MatchString(value) {
		return ""
	}
	return value
}

// matchesResolved reports whether a bare name from the event's resource
// list is the last part of an ARN already resolved from the template, so the
// same resource is not listed again as unknown.
func matchesResolved(name string, arns []string) bool {
	for _, arn := range arns {
		if strings.HasSuffix(arn, "/"+name) || strings.HasSuffix(arn, ":"+name) {
			return true
		}
	}
	return false
}

func contains(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}
