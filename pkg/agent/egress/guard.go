package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/redaction"
)

type PartKind string

const (
	PartSystem     PartKind = "system"
	PartUser       PartKind = "user"
	PartAttachment PartKind = "attachment"
	PartHistory    PartKind = "history"
	PartSeed       PartKind = "seed"
	PartToolResult PartKind = "tool_result"
	PartToolArgs   PartKind = "tool_args"
	PartEmbedding  PartKind = "embedding"
	PartDecider    PartKind = "decider"
)

type Part struct {
	Kind PartKind
	Text string
}

type Request struct {
	Org         string
	Destination string
	Parts       []Part
}

type Result struct {
	Parts      []Part
	Redactions int
}

type Guard interface {
	Apply(context.Context, Request) (Result, error)
}

type Scrubber interface {
	Scrub(string) string
}

var ErrEgressBlocked = errors.New("model egress blocked")

type RedactorGuard struct{ scrubber Scrubber }

func NewRedactorGuard(scrubber Scrubber) *RedactorGuard {
	return &RedactorGuard{scrubber: scrubber}
}

func NewDefaultGuard() *RedactorGuard {
	redactor, _ := redaction.NewRedactor(false, nil)
	return NewRedactorGuard(redactor)
}

func (guard *RedactorGuard) Apply(ctx context.Context, request Request) (result Result, err error) {
	defer func() {
		if recover() != nil {
			result = Result{}
			err = ErrEgressBlocked
		}
	}()
	if guard == nil || guard.scrubber == nil || ctx == nil {
		return Result{}, ErrEgressBlocked
	}
	if err := ctx.Err(); err != nil {
		return Result{}, errors.Join(ErrEgressBlocked, err)
	}
	result.Parts = make([]Part, len(request.Parts))
	for index, part := range request.Parts {
		if err := ctx.Err(); err != nil {
			return Result{}, errors.Join(ErrEgressBlocked, err)
		}
		part.Text, result.Redactions = guard.scrubText(part.Text, result.Redactions)
		result.Parts[index] = part
	}
	return result, nil
}

func (guard *RedactorGuard) scrubText(text string, count int) (string, int) {
	var value any
	if json.Unmarshal([]byte(text), &value) != nil {
		clean := guard.scrubber.Scrub(text)
		if clean != text {
			count++
		}
		return clean, count
	}
	clean, changed := guard.scrubValue(value)
	if changed {
		count++
	}
	encoded, err := json.Marshal(clean)
	if err != nil {
		panic(fmt.Errorf("encode scrubbed egress payload: %w", err))
	}
	return string(encoded), count
}

func (guard *RedactorGuard) scrubValue(value any) (any, bool) {
	changed := false
	switch typed := value.(type) {
	case string:
		clean := guard.scrubber.Scrub(typed)
		return clean, clean != typed
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			var itemChanged bool
			out[index], itemChanged = guard.scrubValue(item)
			changed = changed || itemChanged
		}
		return out, changed
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			cleanKey := guard.scrubber.Scrub(key)
			if cleanKey != key {
				changed = true
			}
			cleanValue, itemChanged := guard.scrubValue(item)
			changed = changed || itemChanged
			out[cleanKey] = cleanValue
		}
		return out, changed
	default:
		return value, false
	}
}

func Apply(ctx context.Context, guard Guard, request Request) (result Result, err error) {
	defer func() {
		if recover() != nil {
			result = Result{}
			err = ErrEgressBlocked
		}
	}()
	if guard == nil || ctx == nil {
		return Result{}, ErrEgressBlocked
	}
	if err := ctx.Err(); err != nil {
		return Result{}, errors.Join(ErrEgressBlocked, err)
	}
	result, err = guard.Apply(ctx, request)
	if err != nil {
		return Result{}, errors.Join(ErrEgressBlocked, err)
	}
	if len(result.Parts) != len(request.Parts) {
		return Result{}, ErrEgressBlocked
	}
	return result, nil
}