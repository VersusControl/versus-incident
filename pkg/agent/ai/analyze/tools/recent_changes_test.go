package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// fakeChangeFeed is a scripted ChangeFeed for tests. It records the
// `since` it was called with so assertions can verify window math.
type fakeChangeFeed struct {
	records  []ChangeRecord
	err      error
	gotSince time.Time
	calls    int
}

func (f *fakeChangeFeed) Changes(_ context.Context, since time.Time) ([]ChangeRecord, error) {
	f.calls++
	f.gotSince = since
	if f.err != nil {
		return nil, f.err
	}
	return f.records, nil
}

func mustInvoke(t *testing.T, rc RecentChanges, args map[string]any) *core.ToolResult {
	t.Helper()
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
		raw = b
	}
	res, err := rc.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	return res
}

func TestRecentChanges_Metadata(t *testing.T) {
	rc := RecentChanges{}
	if rc.Name() != "recent_changes" {
		t.Fatalf("Name = %q", rc.Name())
	}
	if rc.Description() == "" {
		t.Fatal("Description empty")
	}
	schema := rc.ArgsSchema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties missing: %+v", schema)
	}
	for _, k := range []string{"service", "window_minutes"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("schema missing property %q", k)
		}
	}
}

func TestRecentChanges_NilFeedIsCleanMiss(t *testing.T) {
	rc := RecentChanges{Feed: nil}
	res := mustInvoke(t, rc, nil)
	if res.Found {
		t.Fatal("nil feed should be Found=false")
	}
	if res.Tool != "recent_changes" {
		t.Fatalf("Tool = %q", res.Tool)
	}
	if res.Data["count"] != 0 {
		t.Fatalf("count = %v, want 0", res.Data["count"])
	}
	if res.Data["window_minutes"] != recentChangesDefaultWindow {
		t.Fatalf("window_minutes = %v, want %d", res.Data["window_minutes"], recentChangesDefaultWindow)
	}
}

func TestRecentChanges_WindowClamping(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name       string
		window     int
		wantWindow int
		wantMaxAge time.Duration
	}{
		{"default when zero", 0, recentChangesDefaultWindow, time.Duration(recentChangesDefaultWindow) * time.Minute},
		{"default when negative", -5, recentChangesDefaultWindow, time.Duration(recentChangesDefaultWindow) * time.Minute},
		{"passthrough mid", 300, 300, 300 * time.Minute},
		{"cap at max", 100000, recentChangesMaxWindow, time.Duration(recentChangesMaxWindow) * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			feed := &fakeChangeFeed{}
			rc := RecentChanges{Feed: feed}
			args := map[string]any{}
			if tc.window != 0 {
				args["window_minutes"] = tc.window
			}
			res := mustInvoke(t, rc, args)
			if res.Data["window_minutes"] != tc.wantWindow {
				t.Fatalf("window_minutes = %v, want %d", res.Data["window_minutes"], tc.wantWindow)
			}
			// since should be ~now-wantMaxAge.
			gotAge := now.Sub(feed.gotSince)
			if gotAge < tc.wantMaxAge-time.Minute || gotAge > tc.wantMaxAge+time.Minute {
				t.Fatalf("since age = %s, want ~%s", gotAge, tc.wantMaxAge)
			}
		})
	}
}

func TestRecentChanges_ServiceFilterAndOrdering(t *testing.T) {
	now := time.Now().UTC()
	feed := &fakeChangeFeed{records: []ChangeRecord{
		{Timestamp: now.Add(-10 * time.Minute), Service: "api", Kind: "deploy", Summary: "newest api"},
		{Timestamp: now.Add(-30 * time.Minute), Service: "API", Kind: "config", Summary: "older api"},
		{Timestamp: now.Add(-20 * time.Minute), Service: "db", Kind: "deploy", Summary: "db change"},
	}}
	rc := RecentChanges{Feed: feed}

	res := mustInvoke(t, rc, map[string]any{"service": "api"})
	if !res.Found {
		t.Fatal("expected Found=true")
	}
	if res.Data["count"] != 2 {
		t.Fatalf("count = %v, want 2 (case-insensitive api match)", res.Data["count"])
	}
	changes, ok := res.Data["changes"].([]ChangeRecord)
	if !ok {
		t.Fatalf("changes wrong type: %T", res.Data["changes"])
	}
	if len(changes) != 2 {
		t.Fatalf("len(changes) = %d, want 2", len(changes))
	}
	// Newest first.
	if changes[0].Summary != "newest api" || changes[1].Summary != "older api" {
		t.Fatalf("ordering wrong: %+v", changes)
	}
}

func TestRecentChanges_DropsOutOfWindow(t *testing.T) {
	now := time.Now().UTC()
	feed := &fakeChangeFeed{records: []ChangeRecord{
		{Timestamp: now.Add(-5 * time.Minute), Service: "api", Summary: "in"},
		{Timestamp: now.Add(-200 * time.Minute), Service: "api", Summary: "out"},
	}}
	rc := RecentChanges{Feed: feed}
	res := mustInvoke(t, rc, map[string]any{"window_minutes": 60})
	if res.Data["count"] != 1 {
		t.Fatalf("count = %v, want 1", res.Data["count"])
	}
}

func TestRecentChanges_EmptyFeedIsMiss(t *testing.T) {
	rc := RecentChanges{Feed: &fakeChangeFeed{}}
	res := mustInvoke(t, rc, nil)
	if res.Found {
		t.Fatal("empty feed should be Found=false")
	}
	if _, ok := res.Data["changes"]; ok {
		t.Fatal("miss should not carry a changes key")
	}
}

func TestRecentChanges_FeedErrorSurfaces(t *testing.T) {
	rc := RecentChanges{Feed: &fakeChangeFeed{err: context.DeadlineExceeded}}
	_, err := rc.Invoke(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error from feed to surface")
	}
}

func TestRecentChanges_BadArgs(t *testing.T) {
	rc := RecentChanges{Feed: &fakeChangeFeed{}}
	_, err := rc.Invoke(context.Background(), json.RawMessage(`{"window_minutes":"oops"}`))
	if err == nil {
		t.Fatal("expected parse error for non-integer window_minutes")
	}
}

func TestNewGitChangeFeed_EmptyReposNil(t *testing.T) {
	if NewGitChangeFeed(nil) != nil {
		t.Fatal("nil repos should yield nil feed")
	}
	if NewGitChangeFeed([]GitRepo{}) != nil {
		t.Fatal("empty repos should yield nil feed")
	}
	if NewGitChangeFeed([]GitRepo{{URL: "   "}}) != nil {
		t.Fatal("whitespace-only URL should be dropped, yielding nil feed")
	}
}

func TestGitAuthArgs(t *testing.T) {
	t.Run("empty auth yields nothing", func(t *testing.T) {
		args, env := gitAuthArgs(GitRepo{URL: "https://example.com/x.git"})
		if len(args) != 0 || len(env) != 0 {
			t.Fatalf("empty auth should add no args/env, got args=%v env=%v", args, env)
		}
	})
	t.Run("token sets http.extraHeader", func(t *testing.T) {
		args, env := gitAuthArgs(GitRepo{Token: "secret-token"})
		if len(env) != 0 {
			t.Fatalf("token should not set env, got %v", env)
		}
		want := "http.extraHeader=Authorization: Basic " +
			base64.StdEncoding.EncodeToString([]byte("x-access-token:secret-token"))
		if len(args) != 2 || args[0] != "-c" || args[1] != want {
			t.Fatalf("token args = %v, want [-c %q]", args, want)
		}
	})
	t.Run("ssh key sets GIT_SSH_COMMAND", func(t *testing.T) {
		args, env := gitAuthArgs(GitRepo{SSHKeyPath: "/home/u/.ssh/id_ed25519"})
		if len(args) != 0 {
			t.Fatalf("ssh key should not set config args, got %v", args)
		}
		if len(env) != 1 || env[0] != "GIT_SSH_COMMAND=ssh -i /home/u/.ssh/id_ed25519 -o IdentitiesOnly=yes" {
			t.Fatalf("ssh env = %v", env)
		}
	})
	t.Run("both token and ssh key", func(t *testing.T) {
		args, env := gitAuthArgs(GitRepo{Token: "t", SSHKeyPath: "/k"})
		if len(args) != 2 || len(env) != 1 {
			t.Fatalf("expected both auth methods wired, got args=%v env=%v", args, env)
		}
	})
}

func TestServiceFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"git@github.com:acme/web.git", "web"},
		{"https://github.com/acme/api.git", "api"},
		{"https://github.com/acme/api", "api"},
		{"https://github.com/acme/api/", "api"},
		{"/srv/payments", "payments"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := serviceFromURL(tc.url); got != tc.want {
			t.Fatalf("serviceFromURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestParseGitLog(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	older := now.Add(-30 * time.Minute)
	// Two commits, newest first, separated by the record separator. Both
	// are stamped with the supplied service ("api").
	out := gitRecordSep + "abcdef1234567890" + gitFieldSep + now.Format(time.RFC3339) + gitFieldSep + "deploy api" + "\n" +
		gitRecordSep + "0123456789abcdef" + gitFieldSep + older.Format(time.RFC3339) + gitFieldSep + "docs" + "\n"

	recs := parseGitLog(out, "api")
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(recs), recs)
	}
	if recs[0].Summary != "deploy api" || recs[0].Service != "api" {
		t.Fatalf("first record = %+v", recs[0])
	}
	if recs[0].Ref != "abcdef1" {
		t.Fatalf("short SHA = %q, want abcdef1", recs[0].Ref)
	}
	if recs[0].Kind != changesGitKind {
		t.Fatalf("kind = %q, want %q", recs[0].Kind, changesGitKind)
	}
	if !recs[0].Timestamp.Equal(now) {
		t.Fatalf("timestamp = %s, want %s", recs[0].Timestamp, now)
	}
	if recs[1].Service != "api" {
		t.Fatalf("second record service = %q, want api", recs[1].Service)
	}
}

func TestParseGitLog_SkipsMalformed(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	out := gitRecordSep + "shaonly" + "\n" + // header missing date/subject fields
		gitRecordSep + "deadbeef" + gitFieldSep + "not-a-date" + gitFieldSep + "bad ts" + "\n" +
		gitRecordSep + "feedface1234" + gitFieldSep + now.Format(time.RFC3339) + gitFieldSep + "good" + "\n"
	recs := parseGitLog(out, "svc")
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 (only the well-formed commit): %+v", len(recs), recs)
	}
	if recs[0].Summary != "good" || recs[0].Service != "svc" {
		t.Fatalf("survivor = %+v", recs[0])
	}
}

func TestParseGitLog_Empty(t *testing.T) {
	if recs := parseGitLog("", "svc"); len(recs) != 0 {
		t.Fatalf("empty output should yield no records, got %d", len(recs))
	}
}

// initGitRepo builds a throwaway git repo with the given commits (oldest
// first) under a subdirectory named `name` (so the auto-detected service
// is predictable) and returns its path. Each commit stamps the same
// author and committer date so windowing is deterministic. The test is
// skipped when the git binary is unavailable.
func initGitRepo(t *testing.T, name string, commits []struct {
	when    time.Time
	file    string
	message string
}) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	run := func(env []string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run(nil, "init")
	run(nil, "config", "user.email", "test@example.com")
	run(nil, "config", "user.name", "Test")
	for _, c := range commits {
		path := filepath.Join(dir, c.file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(c.message), 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}
		run(nil, "add", ".")
		stamp := c.when.UTC().Format(time.RFC3339)
		run([]string{"GIT_AUTHOR_DATE=" + stamp, "GIT_COMMITTER_DATE=" + stamp},
			"commit", "-m", c.message)
	}
	return dir
}

type commitSpec = struct {
	when    time.Time
	file    string
	message string
}

func TestGitChangeFeed_EndToEnd_ServiceAutoDetect(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := initGitRepo(t, "payments", []commitSpec{
		{now.Add(-300 * time.Minute), "old.go", "old deploy"},
		{now.Add(-10 * time.Minute), "new.go", "new deploy"},
		{now.Add(-5 * time.Minute), "schema.sql", "db change"},
	})

	// No explicit service: it is auto-detected from the repo name. A
	// local path is a valid git URL for the mirror clone.
	feed := newGitChangeFeed([]GitRepo{{URL: repo}}, t.TempDir())
	if feed == nil {
		t.Fatal("non-empty repos should yield a feed")
	}

	// Window covers only the last two commits (the -300m one is excluded).
	recs, err := feed.Changes(context.Background(), now.Add(-60*time.Minute))
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(recs), recs)
	}
	for _, r := range recs {
		if r.Kind != changesGitKind {
			t.Fatalf("kind = %q", r.Kind)
		}
		if r.Service != "payments" {
			t.Fatalf("service = %q, want payments (auto-detected)", r.Service)
		}
	}
}

func TestGitChangeFeed_ExplicitServiceAndMultiRepo(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	apiRepo := initGitRepo(t, "api-src", []commitSpec{
		{now.Add(-8 * time.Minute), "main.go", "api deploy"},
	})
	webRepo := initGitRepo(t, "web", []commitSpec{
		{now.Add(-4 * time.Minute), "index.js", "web deploy"},
	})

	feed := newGitChangeFeed([]GitRepo{
		{URL: apiRepo, Service: "api"}, // explicit service overrides repo name
		{URL: webRepo},                 // auto-detected as "web"
	}, t.TempDir())

	recs, err := feed.Changes(context.Background(), now.Add(-60*time.Minute))
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (aggregated across repos): %+v", len(recs), recs)
	}
	services := map[string]bool{}
	for _, r := range recs {
		services[r.Service] = true
	}
	if !services["api"] || !services["web"] {
		t.Fatalf("services = %v, want both api and web", services)
	}
}

func TestGitChangeFeed_ThroughTool(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	repo := initGitRepo(t, "billing", []commitSpec{
		{now.Add(-3 * time.Minute), "main.go", "latest"},
	})

	rc := RecentChanges{Feed: newGitChangeFeed([]GitRepo{{URL: repo}}, t.TempDir())}
	res := mustInvoke(t, rc, map[string]any{"service": "billing"})
	if !res.Found {
		t.Fatal("expected Found=true from end-to-end git feed")
	}
	if res.Data["count"] != 1 {
		t.Fatalf("count = %v, want 1", res.Data["count"])
	}
}

func TestGitChangeFeed_BadRepoErrors(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	feed := newGitChangeFeed([]GitRepo{{URL: filepath.Join(t.TempDir(), "not-a-repo")}}, t.TempDir())
	if _, err := feed.Changes(context.Background(), time.Now().Add(-time.Hour)); err == nil {
		t.Fatal("expected error when the only repo fails to clone")
	}
}

func TestGitChangeFeed_PartialFailureDegrades(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	good := initGitRepo(t, "good", []commitSpec{
		{now.Add(-2 * time.Minute), "main.go", "good deploy"},
	})

	// One healthy repo plus one broken remote: the broken one is skipped
	// and the healthy repo's records still surface.
	feed := newGitChangeFeed([]GitRepo{
		{URL: filepath.Join(t.TempDir(), "missing")},
		{URL: good},
	}, t.TempDir())

	recs, err := feed.Changes(context.Background(), now.Add(-60*time.Minute))
	if err != nil {
		t.Fatalf("partial failure should not surface an error: %v", err)
	}
	if len(recs) != 1 || recs[0].Service != "good" {
		t.Fatalf("records = %+v, want one record for service good", recs)
	}
}
