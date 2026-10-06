package egress

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestGuardScrubsPartsAndJSONKeys(t *testing.T) {
	secret := "sk-012345678901234567890123"
	result, err := Apply(context.Background(), NewDefaultGuard(), Request{Parts: []Part{
		{Kind: PartUser, Text: "message " + secret},
		{Kind: PartToolResult, Text: `{"` + secret + `":"` + secret + `"}`},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range result.Parts {
		if strings.Contains(part.Text, secret) {
			t.Fatalf("secret survived in %q", part.Text)
		}
	}
	if result.Redactions == 0 {
		t.Fatal("expected redaction count")
	}
}

type panicGuard struct{}

func (panicGuard) Apply(context.Context, Request) (Result, error) { panic("sensitive detail") }

func TestGuardFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, guard := range map[string]Guard{"nil": nil, "panic": panicGuard{}} {
		t.Run(name, func(t *testing.T) {
			if _, err := Apply(context.Background(), guard, Request{}); !errors.Is(err, ErrEgressBlocked) {
				t.Fatalf("error = %v, want ErrEgressBlocked", err)
			}
		})
	}
	if _, err := Apply(ctx, NewDefaultGuard(), Request{}); !errors.Is(err, ErrEgressBlocked) {
		t.Fatalf("cancelled error = %v, want ErrEgressBlocked", err)
	}
}