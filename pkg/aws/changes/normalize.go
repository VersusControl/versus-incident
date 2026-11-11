package changes

import (
	"encoding/json"
	"regexp"
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

// Normalize maps one event onto a change. It returns false for read-only
// events. The raw document, request parameters and response elements are
// dropped; only the template's identifier fields survive, each checked
// against identifierShape.
func Normalize(account, region string, event Event) (model.Change, bool) {
	if strings.EqualFold(event.ReadOnly, "true") {
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

	template, ok := Lookup(event.Source, event.Name)
	if ok {
		fields := extractFields(template.Fields, raw.RequestParameters, raw.ResponseElements)
		change.Template = template.ID
		change.Summary = template.Render(fields)
		if len(fields) > 0 {
			change.Fields = fields
		}
		for _, name := range template.ResourceFields {
			if value := fields[name]; value != "" {
				change.Resources = append(change.Resources, value)
			}
		}
	} else {
		change.Template = GenericTemplateID
		change.Summary = GenericSummary(event.Source, event.Name)
	}
	for _, name := range event.Resources {
		if value := safeIdentifier(name); value != "" && !contains(change.Resources, value) {
			change.Resources = append(change.Resources, value)
		}
	}
	if change.ErrorCode != "" {
		change.Summary = "Failed attempt: " + change.Summary
	}
	return change, true
}

func extractFields(fields []Field, request, response json.RawMessage) map[string]string {
	if len(fields) == 0 {
		return nil
	}
	var requestDoc, responseDoc any
	if len(request) > 0 {
		_ = json.Unmarshal(request, &requestDoc)
	}
	if len(response) > 0 {
		_ = json.Unmarshal(response, &responseDoc)
	}
	out := make(map[string]string, len(fields))
	for _, field := range fields {
		root, rest, found := strings.Cut(field.Path, ".")
		if !found {
			continue
		}
		var document any
		switch root {
		case "requestParameters":
			document = requestDoc
		case "responseElements":
			document = responseDoc
		default:
			continue
		}
		if value := safeIdentifier(walk(document, strings.Split(rest, "."))); value != "" {
			out[field.Name] = value
		}
	}
	return out
}

// walk follows a dotted path and returns a scalar as text. Objects and
// arrays at the end of the path are not identifiers and yield nothing.
func walk(document any, path []string) string {
	current := document
	for _, segment := range path {
		switch typed := current.(type) {
		case map[string]any:
			current = typed[segment]
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(typed) {
				return ""
			}
			current = typed[index]
		default:
			return ""
		}
	}
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

func contains(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}
