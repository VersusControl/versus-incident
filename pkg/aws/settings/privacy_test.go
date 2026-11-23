package settings

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func TestPrivacySelfTestPassesAndCounts24Hours(t *testing.T) {
	store := testStore(t)
	now := time.Date(2026, 11, 3, 14, 15, 0, 0, time.UTC)
	store.setClock(func() time.Time { return now })
	tester := NewSelfTester(store, func() time.Time { return now })

	record, err := tester.Run(context.Background(), "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Passing || record.Leaks24h != 0 {
		t.Fatalf("self-test = %+v, want passing with no leaks", record)
	}
	if !record.LastRunAt.Equal(now) {
		t.Fatalf("last run = %v", record.LastRunAt)
	}

	now = now.Add(25 * time.Hour)
	second, err := tester.Run(context.Background(), "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Leaks24h != 0 {
		t.Fatalf("leaks 24h = %d, want 0", second.Leaks24h)
	}
	stored, err := store.LoadSelfTest("default")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Passing || stored.Leaks24h != 0 {
		t.Fatalf("stored self-test = %+v", stored)
	}
}

func TestPrivacySelfTestHonoursTheAllowlistedCanary(t *testing.T) {
	// A value whose key the allowlist permits is sent on purpose. It must not
	// be reported as a leak, or a valid configuration could never pass.
	for _, key := range []string{"private-notes", "secret-config"} {
		store := testStore(t)
		tester := NewSelfTester(store, time.Now)
		record, err := tester.Run(context.Background(), "default", []string{"app", key})
		if err != nil {
			t.Fatal(err)
		}
		if !record.Passing || record.Leaks24h != 0 {
			t.Fatalf("allowlisting %q reported a leak: %+v", key, record)
		}
	}
}

func TestPrivacySelfTestCatchesNonAllowedLeak(t *testing.T) {
	// The detection half: a value that survives without an allowlist sanction
	// is a leak.
	projection := strings.Join([]string{canaryEnvValue, canaryRequestBody, canaryConfigTagValue}, "|")
	if leaks := countLeaks(projection, []string{"private-notes"}); leaks != 3 {
		t.Fatalf("detected %d leaks, want 3", leaks)
	}
	// Allowlisting the key that carries a canary removes exactly that one.
	if leaks := countLeaks(projection, []string{"secret-config"}); leaks != 2 {
		t.Fatalf("allowlist did not exempt its own value: %d leaks", leaks)
	}
}

func TestPrivacySelfTestUsesConfigWinsAllowlist(t *testing.T) {
	store := testStore(t)
	connector := NewService(store,
		WithConfig(func() config.AWSConnectorConfig {
			return config.AWSConnectorConfig{
				Enable: true,
				Tags:   config.AWSTagsConfig{ValueAllowlist: []string{"app", "private-notes"}},
			}
		}),
		WithClock(time.Now),
	)
	connector.runSelfTest(context.Background())
	record, err := store.LoadSelfTest("default")
	if err != nil {
		t.Fatal(err)
	}
	if !record.Passing || record.Leaks24h != 0 {
		t.Fatalf("config-allowed tag value was treated as a leak: %+v", record)
	}

	// A saved wizard allowlist that permits the other canary must not override
	// the config: the card and the self-test must agree.
	if err := store.SavePrivacy("default", privacyRecord{TagValueAllowlist: []string{"secret-config"}}); err != nil {
		t.Fatal(err)
	}
	connector.runSelfTest(context.Background())
	record, err = store.LoadSelfTest("default")
	if err != nil {
		t.Fatal(err)
	}
	if !record.Passing {
		t.Fatalf("saved allowlist overrode the config allowlist: %+v", record)
	}
}

func TestPrivacyFixtureRedactsEnvKeysAndTrailBodies(t *testing.T) {
	store := testStore(t)
	tester := NewSelfTester(store, time.Now)
	projection, err := tester.project(context.Background(), "default", DefaultTagValueAllowlist())
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range canaries {
		if containsString(projection, canary) {
			t.Fatalf("projection kept canary %q: %s", canary, projection)
		}
	}
	if !containsString(projection, "checkout") {
		t.Fatal("allowlisted tag value was dropped")
	}
}

// A list the user saves wins, even an empty one: saving [] means no tag
// values at all, and a self-test run after it keeps it empty.
func TestSavedEmptyAllowlistMeansNoTagValues(t *testing.T) {
	store := testStore(t)
	connector := NewService(store, WithConfig(func() config.AWSConnectorConfig { return config.AWSConnectorConfig{Enable: true} }))
	org := connector.storageOrg()
	if err := connector.SavePrivacy(org, []string{}); err != nil {
		t.Fatal(err)
	}
	if got := connector.TagValueAllowlist(); len(got) != 0 {
		t.Fatalf("after saving an empty list: %v", got)
	}
	connector.runSelfTest(context.Background())
	if _, err := connector.selfTester.Run(context.Background(), org, nil); err != nil {
		t.Fatal(err)
	}
	got := connector.TagValueAllowlist()
	if len(got) != 0 {
		t.Fatalf("after the self-test: %v", got)
	}
	allowed := map[string]bool{}
	for _, key := range got {
		allowed[key] = true
	}
	for _, key := range []string{"app", "App", "team", "service"} {
		if model.TagAllowed(allowed, key) {
			t.Fatalf("%s is still allowlisted", key)
		}
	}
	privacy, _ := store.LoadPrivacy(org)
	if !privacy.Saved || len(privacy.TagValueAllowlist) != 0 {
		t.Fatalf("stored = %+v", privacy)
	}
}

// A self-test on a fresh install tests the default but saves no list: the
// default is never stored as if the user had chosen it.
func TestSelfTestSavesNoUserAllowlist(t *testing.T) {
	store := testStore(t)
	connector := NewService(store, WithConfig(func() config.AWSConnectorConfig { return config.AWSConnectorConfig{Enable: true} }))
	org := connector.storageOrg()
	connector.runSelfTest(context.Background())
	privacy, err := store.LoadPrivacy(org)
	if err != nil {
		t.Fatal(err)
	}
	if privacy.Saved || len(privacy.TagValueAllowlist) != 0 || len(privacy.Samples) != 1 {
		t.Fatalf("stored after the self-test = %+v", privacy)
	}
	if _, source := connector.allowlistWithSource(org); source != sourceDefault {
		t.Fatalf("source = %s, want the default", source)
	}
}

// The old five-key default stored without the saved marker was written by
// an older self-test, not chosen by the user: in any order or case it
// follows the current default.
func TestOldDefaultWithoutTheSavedMarkerFollowsTheDefault(t *testing.T) {
	for _, stored := range [][]string{{"app", "team", "env", "owner", "cost-center"}, {"Cost-Center", "OWNER", "env", "Team", "app"}, {}, nil} {
		store := testStore(t)
		connector := NewService(store, WithConfig(func() config.AWSConnectorConfig { return config.AWSConnectorConfig{Enable: true} }))
		org := connector.storageOrg()
		if err := store.SavePrivacy(org, privacyRecord{TagValueAllowlist: stored}); err != nil {
			t.Fatal(err)
		}
		if got, want := connector.TagValueAllowlist(), DefaultTagValueAllowlist(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("stored %v: allowlist = %v, want the default %v", stored, got, want)
		}
	}
}

// Any other list stored without the saved marker was saved by the user
// before the marker existed: it is kept, never widened to the default, and
// a self-test leaves it alone.
func TestListSavedBeforeTheMarkerIsKept(t *testing.T) {
	for _, stored := range [][]string{{"team"}, {"app", "team", "env", "owner"}, {"app", "team", "env", "owner", "cost-center", "Name"}} {
		store := testStore(t)
		connector := NewService(store, WithConfig(func() config.AWSConnectorConfig { return config.AWSConnectorConfig{Enable: true} }))
		org := connector.storageOrg()
		if err := store.SavePrivacy(org, privacyRecord{TagValueAllowlist: stored}); err != nil {
			t.Fatal(err)
		}
		connector.runSelfTest(context.Background())
		got, source := connector.allowlistWithSource(org)
		if strings.Join(got, ",") != strings.Join(stored, ",") || source != string(SourceWizard) {
			t.Fatalf("stored %v: allowlist = %v from %s", stored, got, source)
		}
	}
}

// interleavingProvider runs hook on the first read of key, as if another
// replica wrote in between a reader's read and its write.
type interleavingProvider struct {
	storage.Provider
	cas  storage.BlobCAS
	key  string
	hook func()
	once sync.Once
}

func (p *interleavingProvider) ReadBlob(name string) ([]byte, error) {
	data, err := p.Provider.ReadBlob(name)
	if name == p.key {
		p.once.Do(p.hook)
	}
	return data, err
}

func (p *interleavingProvider) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	return p.cas.CompareAndSwapBlob(name, expected, replacement)
}

// A list saved while a self-test runs is kept: the self-test writes only
// its samples, re-reading the record when another writer changed it.
func TestSelfTestKeepsAnAllowlistSavedMeanwhile(t *testing.T) {
	memory := storage.NewMemory()
	cas, _ := memory.(storage.BlobCAS)
	org := tenancy.DefaultOrgScope().Write
	other := NewService(NewStore(memory, tenancy.DefaultOrgScope()),
		WithConfig(func() config.AWSConnectorConfig { return config.AWSConnectorConfig{Enable: true} }))
	provider := &interleavingProvider{Provider: memory, cas: cas, key: PrivacyKey(org), hook: func() {
		if err := other.SavePrivacy(org, []string{"team"}); err != nil {
			t.Errorf("interleaved save: %v", err)
		}
	}}
	store := NewStore(provider, tenancy.DefaultOrgScope())
	tester := NewSelfTester(store, time.Now)
	if _, err := tester.Run(context.Background(), org, []string{"app"}); err != nil {
		t.Fatal(err)
	}
	privacy, err := store.LoadPrivacy(org)
	if err != nil {
		t.Fatal(err)
	}
	if !privacy.Saved || strings.Join(privacy.TagValueAllowlist, ",") != "team" || len(privacy.Samples) != 1 {
		t.Fatalf("stored = %+v", privacy)
	}
}

// The self-test matches keys in any case, like the real redaction: "Private-Notes"
// on the allowlist sends the private-notes canary on purpose.
func TestSelfTestMatchesAllowlistKeysInAnyCase(t *testing.T) {
	tester := NewSelfTester(testStore(t), time.Now)
	record, err := tester.Run(context.Background(), "default", []string{"PRIVATE-NOTES"})
	if err != nil || !record.Passing {
		t.Fatalf("record = %+v, %v", record, err)
	}
	if leaks := countLeaks("canary-tag-7f3a91", []string{"Private-Notes"}); leaks != 0 {
		t.Fatalf("leaks = %d", leaks)
	}
	if leaks := countLeaks("canary-tag-7f3a91", []string{"notes"}); leaks != 1 {
		t.Fatalf("leaks with another key = %d", leaks)
	}
}
