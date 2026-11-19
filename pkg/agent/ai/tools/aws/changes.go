package aws

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/settings"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// ChangesToolName is the change-list tool.
const ChangesToolName = "get_aws_changes"

// ChangeAnswerer is the change list the Changes page reads: the same
// filters, items, gaps and likely cause, from stored state only.
type ChangeAnswerer interface {
	Answer(ctx context.Context, request snapshot.FeedRequest) (snapshot.ChangesAnswer, error)
}

// ChangesFunc returns the change list, or nil when it is not set up.
type ChangesFunc func() ChangeAnswerer

// Change-list limits for the model.
const (
	changesDefaultLimit = 20
	changesMaxLimit     = 50
	changesMaxGaps      = 20
	changesUI           = "/aws/changes"
)

var (
	regionShape = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	// emailToken and accessKeyToken mask what may still hide in a
	// summary or field value: an email address or an access key ID.
	emailToken     = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	accessKeyToken = regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)
)

// NewChanges builds the change-list tool over one source.
func NewChanges(feed ChangesFunc) []core.Tool {
	if feed == nil {
		return nil
	}
	return []core.Tool{&changesTool{feed: feed}}
}

type changesTool struct {
	feed ChangesFunc
}

func (*changesTool) Name() string        { return ChangesToolName }
func (*changesTool) DisplayName() string { return displayNames[ChangesToolName] }
func (*changesTool) Description() string { return descriptions[ChangesToolName] }

func (*changesTool) ArgsSchema() map[string]any {
	return (&tool{name: ChangesToolName}).ArgsSchema()
}

type changesArguments struct {
	Window      string `json:"window"`
	Until       string `json:"until"`
	AccountID   string `json:"account_id"`
	Region      string `json:"region"`
	ActorClass  string `json:"actor_class"`
	Service     string `json:"service"`
	Resource    string `json:"resource"`
	Text        string `json:"q"`
	ShowRoutine bool   `json:"show_routine"`
	Cursor      string `json:"cursor"`
	Limit       int    `json:"limit"`
}

// Invoke answers from the stored change history through the same feed as
// the Changes page. It never says "no changes" unless the window is
// complete.
func (t *changesTool) Invoke(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	if !core.CallerAuthorized(ctx, core.PermissionInfrastructureView) {
		return core.UnavailableToolResult(ChangesToolName, "infrastructure:view permission is required"), nil
	}
	var args changesArguments
	if len(raw) != 0 && string(raw) != "null" {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&args); err != nil {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "invalid AWS changes arguments", err)
		}
	}
	request, locked, err := args.request()
	if err != nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "invalid AWS changes arguments", err)
	}
	if locked {
		return windowLocked(), nil
	}
	var feed ChangeAnswerer
	if t.feed != nil {
		feed = t.feed()
	}
	if feed == nil {
		return core.UnavailableToolResult(ChangesToolName, "the AWS change history is not set up"), nil
	}
	var data map[string]any
	var answer snapshot.ChangesAnswer
	for _, limit := range shrinkingLimits(request.Limit) {
		request.Limit = limit
		answer, err = feed.Answer(ctx, request)
		switch {
		case errors.Is(err, snapshot.ErrWindowLocked):
			return windowLocked(), nil
		case errors.Is(err, snapshot.ErrCursorExpired):
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "the cursor has expired; call again without it to read the first page", err)
		case errors.Is(err, snapshot.ErrBadFeedQuery):
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "invalid AWS changes arguments", err)
		case err != nil:
			return nil, err
		}
		var fits bool
		if data, fits = ChangesView(answer); fits {
			break
		}
	}
	found := answer.Summary.Total > 0 || answer.Summary.Errors > 0 || answer.Summary.Routine > 0 || len(answer.Items) > 0
	return &core.ToolResult{Tool: ChangesToolName, Found: found, Data: data}, nil
}

func (args changesArguments) request() (snapshot.FeedRequest, bool, error) {
	request := snapshot.FeedRequest{Account: args.AccountID, Region: args.Region, ActorClass: args.ActorClass, Service: args.Service,
		Resource: args.Resource, Text: args.Text, ShowRoutine: args.ShowRoutine, Cursor: args.Cursor, Limit: args.Limit}
	switch args.Window {
	case "", "24h":
		request.Window = snapshot.FeedWindowDay
	case "1h":
		request.Window = time.Hour
	default:
		if parsed, err := time.ParseDuration(args.Window); err == nil && parsed > snapshot.MaxFeedWindow {
			return request, true, nil
		}
		if strings.HasSuffix(args.Window, "d") {
			return request, true, nil
		}
		return request, false, errors.New("window must be 1h or 24h")
	}
	if args.Until != "" {
		until, err := time.Parse(time.RFC3339, args.Until)
		if err != nil {
			return request, false, errors.New("until must be an RFC 3339 time")
		}
		request.Until = until
	}
	if args.AccountID != "" && !settings.ValidAccountID(args.AccountID) {
		return request, false, errors.New("account_id must be a 12-digit account ID")
	}
	if args.Region != "" && !regionShape.MatchString(args.Region) {
		return request, false, errors.New("region is not a Region name")
	}
	if args.Limit < 0 || args.Limit > changesMaxLimit {
		return request, false, errors.New("limit must be between 1 and " + strconv.Itoa(changesMaxLimit))
	}
	if request.Limit == 0 {
		request.Limit = changesDefaultLimit
	}
	return request, false, nil
}

func shrinkingLimits(limit int) []int {
	out := []int{limit}
	for limit > 1 {
		limit /= 2
		out = append(out, limit)
	}
	return out
}

func windowLocked() *core.ToolResult {
	return &core.ToolResult{Tool: ChangesToolName, Found: false, Reason: "window_locked",
		Data: map[string]any{"error": "window_locked", "tier": "enterprise", "max_window": "24h",
			"answer": "Windows longer than 24 hours are an Enterprise feature; ask for 1h or 24h."}}
}

// ChangesView is the answer as the model sees it: the API shape with every
// string masked (email addresses, access key IDs), an answer line that
// never claims "no changes" over an incomplete window, the gap list capped
// and the grid buckets dropped when the view would not fit. fits is false
// when the view is still over MaxModelViewBytes.
func ChangesView(answer snapshot.ChangesAnswer) (map[string]any, bool) {
	encoded, err := json.Marshal(answer)
	if err != nil {
		return map[string]any{"answer": "The change history could not be read."}, true
	}
	var view map[string]any
	_ = json.Unmarshal(encoded, &view)
	view = mask(view).(map[string]any)
	view["answer"] = answerLine(answer)
	view["ref"] = "aws://changes"
	view["links"] = map[string]any{"ui": changesUI}
	if gaps, ok := view["gaps"].([]any); ok && len(gaps) > changesMaxGaps {
		view["gaps"] = gaps[:changesMaxGaps]
		view["gaps_omitted"] = len(gaps) - changesMaxGaps
	}
	if size(view) <= MaxModelViewBytes {
		return view, true
	}
	delete(view, "buckets")
	view["buckets_omitted"] = true
	return view, size(view) <= MaxModelViewBytes
}

func size(view map[string]any) int {
	encoded, _ := json.Marshal(view)
	return len(encoded)
}

// answerLine states what the window shows. "No changes" is said only over
// a complete window, and then only up to settled_until.
func answerLine(answer snapshot.ChangesAnswer) string {
	total := strconv.FormatInt(answer.Summary.Total, 10)
	settled := answer.Window.SettledUntil.UTC().Format(time.RFC3339)
	switch {
	case answer.Complete && answer.Summary.Total == 0:
		return "No changes from " + answer.Window.Since.UTC().Format(time.RFC3339) + " to " + settled + "; anything newer is still arriving."
	case answer.Complete:
		return total + " changes, complete up to " + settled + "; anything newer is still arriving."
	case answer.Summary.Total == 0:
		return "Changes unknown for part of this window (see gaps): no change was read, but that does not mean nothing changed."
	}
	return "At least " + total + " changes; part of this window was not read (see gaps), so the list is not complete."
}

// mask replaces email addresses and access key IDs in every string.
func mask(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			typed[key] = mask(item)
		}
		return typed
	case []any:
		for index, item := range typed {
			typed[index] = mask(item)
		}
		return typed
	case string:
		return MaskText(typed)
	}
	return value
}

// MaskText hides email addresses and access key IDs in one string.
func MaskText(text string) string {
	text = emailToken.ReplaceAllString(text, changes.MaskedEmail)
	return accessKeyToken.ReplaceAllString(text, "(access key hidden)")
}
