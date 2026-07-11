package services

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestIntakeSettings_DefaultsOnWhenAbsent(t *testing.T) {
	// nil store → default ON (auto-resolve webhook incidents).
	if got := LoadIntakeSettings(nil); !got.AutoResolveWebhook {
		t.Fatalf("nil-store default = %+v, want auto_resolve_webhook true", got)
	}
	// fresh store with no blob → same default ON.
	if got := LoadIntakeSettings(storage.NewMemory()); !got.AutoResolveWebhook {
		t.Fatalf("empty-store default = %+v, want auto_resolve_webhook true", got)
	}
}

func TestIntakeSettings_SaveLoadRoundTrip(t *testing.T) {
	st := storage.NewMemory()

	// Persisting an explicit false round-trips faithfully (no omitempty on the
	// bool), so an operator CAN turn the default-on behaviour off.
	if err := SaveIntakeSettings(st, IntakeSettings{AutoResolveWebhook: false}); err != nil {
		t.Fatalf("SaveIntakeSettings: %v", err)
	}
	if got := LoadIntakeSettings(st); got.AutoResolveWebhook {
		t.Fatalf("after saving false, load = %+v, want false", got)
	}

	// Flipping it back on round-trips too.
	if err := SaveIntakeSettings(st, IntakeSettings{AutoResolveWebhook: true}); err != nil {
		t.Fatalf("SaveIntakeSettings: %v", err)
	}
	if got := LoadIntakeSettings(st); !got.AutoResolveWebhook {
		t.Fatalf("after saving true, load = %+v, want true", got)
	}
}

func TestSaveIntakeSettings_NoStorage(t *testing.T) {
	if err := SaveIntakeSettings(nil, IntakeSettings{}); err != ErrIntakeNoStorage {
		t.Fatalf("err = %v, want ErrIntakeNoStorage", err)
	}
}

// TestShouldAutoResolveWebhook is the pure decision matrix for the webhook
// auto-resolve gate: only a not-already-resolved, webhook-origin incident with
// the toggle on is auto-resolved. SNS/SQS transports and agent emits (whose
// durable Source is never "webhook") are excluded.
func TestShouldAutoResolveWebhook(t *testing.T) {
	on := IntakeSettings{AutoResolveWebhook: true}
	off := IntakeSettings{AutoResolveWebhook: false}

	cases := []struct {
		name            string
		alreadyResolved bool
		source          string
		settings        IntakeSettings
		want            bool
	}{
		{"webhook enabled not-resolved", false, "webhook", on, true},
		{"webhook disabled", false, "webhook", off, false},
		{"webhook already resolved", true, "webhook", on, false},
		{"sns transport", false, "sns", on, false},
		{"sqs transport", false, "sqs", on, false},
		{"agent source", false, "agent:elasticsearch:app", on, false},
		{"bare agent source", false, "agent", on, false},
		{"empty source", false, "", on, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldAutoResolveWebhook(tc.alreadyResolved, tc.source, tc.settings); got != tc.want {
				t.Fatalf("shouldAutoResolveWebhook(%v, %q, %+v) = %v, want %v",
					tc.alreadyResolved, tc.source, tc.settings, got, tc.want)
			}
		})
	}
}
