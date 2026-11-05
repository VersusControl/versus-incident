package settings

import (
	"context"
	"encoding/json"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent/egress"
	"github.com/VersusControl/versus-incident/pkg/redaction"
)

// Canary values the self-test plants in its fixture. Each one must be absent
// from the projection the model would receive unless an allowlist explicitly
// permits the key that carries it.
const (
	canaryTagValue       = "canary-tag-7f3a91"
	canaryConfigTagValue = "canary-config-4d2e18"
	canaryEnvValue       = "canary-env-2b8c44"
	canaryAccessKey      = "AKIAIOSFODNN7EXAMPLE"
	canaryRequestBody    = "canary-request-c0ffee"
	canarySessionToken   = "canary-session-91de20"
)

// canaries is every planted value, in fixture order.
var canaries = []string{canaryTagValue, canaryConfigTagValue, canaryEnvValue, canaryAccessKey, canaryRequestBody, canarySessionToken}

// canaryTagKeys maps a tag-value canary to the fixture tag key that carries
// it. A key the allowlist permits is sent on purpose, so its value is not a
// leak; every other canary has no key and can never be allowlisted.
var canaryTagKeys = map[string]string{
	canaryTagValue:       "private-notes",
	canaryConfigTagValue: "secret-config",
}

// selfTestExtraRules close the shapes the fixture models but the default
// redactor does not: JSON secret keys and AWS environment assignments.
var selfTestExtraRules = []string{
	`(?i)"(?:password|passwd|secret|session[_-]?token|secret[_-]?access[_-]?key|api[_-]?key)"\s*:\s*"[^"]*"`,
	`(?i)\b(?:AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN|DATABASE_URL|OPENAI_API_KEY)\s*=\s*\S+`,
}

// SelfTester proves the outbound projection scrubs a fixed set of canaries.
type SelfTester struct {
	store *Store
	now   func() time.Time
	guard egress.Guard
}

// NewSelfTester builds the privacy self-test over a store.
func NewSelfTester(store *Store, now func() time.Time) *SelfTester {
	if now == nil {
		now = time.Now
	}
	return &SelfTester{store: store, now: now, guard: egress.NewDefaultGuard()}
}

// Run projects the fixture, counts surviving canaries, and records the answer.
// A nil allowlist loads the saved value (or the default) and records it; an
// explicit allowlist is used as-is, which is how a config-managed allowlist
// stays read-only.
func (tester *SelfTester) Run(ctx context.Context, org string, allowlist []string) (SelfTestRecord, error) {
	privacySelfTestMu.Lock()
	defer privacySelfTestMu.Unlock()

	persistAllowlist := len(allowlist) == 0
	if persistAllowlist {
		privacy, err := tester.store.LoadPrivacy(org)
		if err != nil {
			return SelfTestRecord{}, err
		}
		allowlist = privacy.TagValueAllowlist
		if len(allowlist) == 0 {
			allowlist = DefaultTagValueAllowlist()
		}
	}
	allowlist = SanitizeAllowlist(allowlist)

	projection, err := tester.project(ctx, org, allowlist)
	if err != nil {
		return SelfTestRecord{}, err
	}
	leaks := countLeaks(projection, allowlist)

	now := tester.now().UTC()
	privacy, err := tester.store.LoadPrivacy(org)
	if err != nil {
		return SelfTestRecord{}, err
	}
	if persistAllowlist {
		privacy.TagValueAllowlist = allowlist
	}
	samples := make([]leakSample, 0, len(privacy.Samples)+1)
	cutoff := now.Add(-24 * time.Hour)
	for _, sample := range privacy.Samples {
		if sample.At.After(cutoff) {
			samples = append(samples, sample)
		}
	}
	samples = append(samples, leakSample{At: now, Leaks: leaks})
	privacy.Samples = samples

	total := 0
	for _, sample := range samples {
		total += sample.Leaks
	}
	record := SelfTestRecord{LastRunAt: now, Passing: leaks == 0, Leaks24h: total}
	if err := tester.store.SavePrivacy(org, privacy); err != nil {
		return SelfTestRecord{}, err
	}
	if err := tester.store.SaveSelfTest(org, record); err != nil {
		return SelfTestRecord{}, err
	}
	return record, nil
}

// countLeaks counts canaries that survived the projection for a reason the
// allowlist does not sanction. A tag value whose key is allowlisted is sent on
// purpose and is not a leak.
func countLeaks(projection string, allowlist []string) int {
	leaks := 0
	for _, canary := range canaries {
		if key, ok := canaryTagKeys[canary]; ok && containsValue(allowlist, normalizeAllowed(key)) {
			continue
		}
		if containsString(projection, canary) {
			leaks++
		}
	}
	return leaks
}

// project builds the fixture projection and runs it through the redactor, the
// tag allowlist, and the egress guard — the same layers real tool output
// crosses.
func (tester *SelfTester) project(ctx context.Context, org string, allowlist []string) (string, error) {
	redactor, _ := redaction.NewRedactor(false, selfTestExtraRules)
	filteredTags := make(map[string]string)
	for key, value := range privacyFixtureTags() {
		if containsValue(allowlist, normalizeAllowed(key)) {
			filteredTags[key] = value
		}
	}
	environment := []string{
		"AWS_ACCESS_KEY_ID=" + canaryAccessKey,
		"AWS_SECRET_ACCESS_KEY=" + canaryEnvValue,
		"AWS_SESSION_TOKEN=" + canarySessionToken,
	}
	for index := range environment {
		environment[index] = redactor.Scrub(environment[index])
	}
	requestParameters := make(map[string]any)
	for key, value := range map[string]string{
		"password":     canaryRequestBody,
		"sessionToken": canarySessionToken,
	} {
		if sensitiveJSONKey(key) {
			requestParameters[key] = "<REDACTED>"
			continue
		}
		requestParameters[key] = redactor.Scrub(value)
	}
	payload := map[string]any{
		"tags":        filteredTags,
		"environment": environment,
		"cloudtrail": map[string]any{
			"eventSource":       "iam.amazonaws.com",
			"requestParameters": requestParameters,
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	scrubbed := redactor.Scrub(string(encoded))
	result, err := egress.Apply(ctx, tester.guard, egress.Request{
		Org:         org,
		Destination: "privacy-selftest",
		Parts:       []egress.Part{{Kind: egress.PartToolResult, Text: scrubbed}},
	})
	if err != nil {
		return "", err
	}
	projection := ""
	for _, part := range result.Parts {
		projection += part.Text
	}
	return projection, nil
}

// privacyFixtureTags is the tag set the self-test projects. Only allowlisted
// keys may reach the model; private-notes carries the canary.
// sensitiveJSONKey reports a projected map key whose value is a secret by
// shape, so the projection drops it instead of relying on a text rule.
func sensitiveJSONKey(key string) bool {
	switch normalizeAllowed(key) {
	case "password", "passwd", "secret", "secret_access_key", "secretaccesskey",
		"session_token", "sessiontoken", "token", "api_key", "apikey":
		return true
	}
	return false
}

func privacyFixtureTags() map[string]string {
	return map[string]string{
		"app":           "checkout",
		"team":          "payments",
		"env":           "prod",
		"owner":         "on-call",
		"cost-center":   "cc-1042",
		"private-notes": canaryTagValue,
		"secret-config": canaryConfigTagValue,
	}
}

func containsString(value, needle string) bool {
	if needle == "" {
		return false
	}
	for index := 0; index+len(needle) <= len(value); index++ {
		if value[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}

func containsValue(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func normalizeAllowed(value string) string {
	out := make([]byte, 0, len(value))
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character >= 'A' && character <= 'Z' {
			character += 'a' - 'A'
		}
		out = append(out, character)
	}
	return string(out)
}

// selfTestLoop runs the self-test at boot and then hourly until shutdown.
func (conn *Service) selfTestLoop(ctx context.Context) {
	conn.runSelfTest(ctx)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			conn.runSelfTest(ctx)
		}
	}
}

func (conn *Service) runSelfTest(ctx context.Context) {
	if conn.selfTester == nil {
		return
	}
	org := conn.storageOrg()
	allowlist, source := conn.allowlistWithSource(org)
	if source == string(SourceConfig) {
		_, _ = conn.selfTester.Run(ctx, org, allowlist)
		return
	}
	_, _ = conn.selfTester.Run(ctx, org, nil)
}
