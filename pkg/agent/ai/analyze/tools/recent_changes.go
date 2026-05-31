package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// ChangeRecord is one change derived from a git commit: a deploy,
// config change, or feature-flag flip recorded in the repository's
// history.
type ChangeRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Service   string    `json:"service"`
	Kind      string    `json:"kind"`
	Summary   string    `json:"summary"`
	Ref       string    `json:"ref,omitempty"`
}

// ChangeFeed is the read-only feed of recent changes the recent_changes
// tool reads from. It is declared as a local interface so the tools
// package stays decoupled from config and pkg/agent.
type ChangeFeed interface {
	// Changes returns every change record at or after `since`. A missing
	// or empty feed yields an empty slice and a nil error (a clean miss,
	// never a hard failure).
	Changes(ctx context.Context, since time.Time) ([]ChangeRecord, error)
}

// RecentChanges surfaces recent deploys, config changes, and
// feature-flag flips so the analyze agent can correlate an incident with
// what changed just before it. It is strictly read-only.
type RecentChanges struct {
	Feed ChangeFeed
}

const (
	recentChangesDefaultWindow = 120
	recentChangesMaxWindow     = 1440
)

// Name implements core.AnalyzeTool.
func (RecentChanges) Name() string { return "recent_changes" }

// Description implements core.AnalyzeTool.
func (RecentChanges) Description() string {
	return "List recent deploys, config changes, and feature-flag flips from the change feed within a time window, newest first. Optionally filter by service. Use it to correlate an incident with what changed just before it."
}

// ArgsSchema implements core.AnalyzeTool.
func (RecentChanges) ArgsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"service": map[string]any{
				"type":        "string",
				"description": "Optional service name to filter changes by (case-insensitive exact match).",
			},
			"window_minutes": map[string]any{
				"type":        "integer",
				"description": "Look back this many minutes from now. Default 120, max 1440.",
			},
		},
	}
}

type recentChangesArgs struct {
	Service       string `json:"service"`
	WindowMinutes int    `json:"window_minutes"`
}

// Invoke implements core.AnalyzeTool.
func (rc RecentChanges) Invoke(ctx context.Context, args json.RawMessage) (*core.ToolResult, error) {
	var a recentChangesArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("recent_changes: parse args: %w", err)
		}
	}
	if a.WindowMinutes <= 0 {
		a.WindowMinutes = recentChangesDefaultWindow
	}
	if a.WindowMinutes > recentChangesMaxWindow {
		a.WindowMinutes = recentChangesMaxWindow
	}

	// An unconfigured feed is a clean miss, never an error: the model
	// simply learns there is no change data to correlate against.
	if rc.Feed == nil {
		return rc.miss(a), nil
	}

	now := time.Now().UTC()
	since := now.Add(-time.Duration(a.WindowMinutes) * time.Minute)

	records, err := rc.Feed.Changes(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("recent_changes: read feed: %w", err)
	}

	out := make([]ChangeRecord, 0, len(records))
	for _, r := range records {
		if r.Timestamp.Before(since) {
			continue
		}
		if a.Service != "" && !strings.EqualFold(r.Service, a.Service) {
			continue
		}
		out = append(out, r)
	}

	// Newest first.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Timestamp.After(out[j].Timestamp)
	})

	if len(out) == 0 {
		return rc.miss(a), nil
	}

	return &core.ToolResult{
		Tool:  RecentChanges{}.Name(),
		Found: true,
		Data: map[string]any{
			"count":          len(out),
			"window_minutes": a.WindowMinutes,
			"service":        a.Service,
			"changes":        out,
		},
	}, nil
}

// miss is the uniform empty envelope for an unconfigured feed or a
// window/service combination that matched nothing.
func (RecentChanges) miss(a recentChangesArgs) *core.ToolResult {
	return &core.ToolResult{
		Tool:  RecentChanges{}.Name(),
		Found: false,
		Data: map[string]any{
			"count":          0,
			"window_minutes": a.WindowMinutes,
			"service":        a.Service,
		},
	}
}

// changesGitKind labels every record sourced from a git commit.
const changesGitKind = "commit"

// Git output field/record separators. ASCII unit/record separators are
// used because they never appear in commit subjects or file paths, so
// parsing stays robust without escaping.
const (
	gitFieldSep  = "\x1f"
	gitRecordSep = "\x1e"
)

// gitChangeFeed reads change records from one or more remote git
// repositories' commit histories on every call. It shells out to the
// `git` binary (which must be on PATH) so no Go git dependency is pulled
// in. Each repository is mirror-cloned into a local cache directory on
// first use and fetched on subsequent lookups; reading per-call keeps the
// feed fresh and the tool stateless.
type gitChangeFeed struct {
	repos    []GitRepo
	cacheDir string
}

// GitRepo describes one remote git repository the change feed reads.
type GitRepo struct {
	// URL is the remote clone URL (https or scp-like git@host:org/repo).
	URL string
	// Branch optionally pins which branch to read; empty = default HEAD.
	Branch string
	// Service maps every commit in this repository to a service name.
	// Empty derives the service from the repository name in the URL.
	Service string
	// Token is an HTTPS access token / PAT used to authenticate to the
	// remote. Empty relies on the ambient git credentials.
	Token string
	// SSHKeyPath is the path to a private SSH key used for ssh / scp-like
	// remotes. Empty relies on the ambient SSH configuration.
	SSHKeyPath string
}

// gitCacheDirName is the subdirectory of the OS temp dir where remote
// repositories are mirror-cloned. It is not configurable.
const gitCacheDirName = "versus-incident-git-cache"

// NewGitChangeFeed returns a ChangeFeed backed by the given remote git
// repositories. Repos with an empty URL are ignored; an empty (or
// all-empty) list yields nil so analyzetools.Default omits the
// recent_changes tool. The `git` binary must be available on PATH.
func NewGitChangeFeed(repos []GitRepo) ChangeFeed {
	return newGitChangeFeed(repos, filepath.Join(os.TempDir(), gitCacheDirName))
}

// newGitChangeFeed is the cache-dir-injectable constructor used by tests.
func newGitChangeFeed(repos []GitRepo, cacheDir string) ChangeFeed {
	cleaned := make([]GitRepo, 0, len(repos))
	for _, r := range repos {
		if strings.TrimSpace(r.URL) == "" {
			continue
		}
		cleaned = append(cleaned, GitRepo{
			URL:        strings.TrimSpace(r.URL),
			Branch:     strings.TrimSpace(r.Branch),
			Service:    strings.TrimSpace(r.Service),
			Token:      strings.TrimSpace(r.Token),
			SSHKeyPath: strings.TrimSpace(r.SSHKeyPath),
		})
	}
	if len(cleaned) == 0 {
		return nil
	}
	return &gitChangeFeed{repos: cleaned, cacheDir: cacheDir}
}

// Changes implements ChangeFeed. It reads each configured repository's
// commit history since the given time and aggregates the records. A
// per-repository failure (e.g. a transient network error) is skipped so
// one broken remote cannot blind the whole feed; an error surfaces only
// when every repository failed. No commits in the window is a clean empty
// result, not an error.
func (f *gitChangeFeed) Changes(ctx context.Context, since time.Time) ([]ChangeRecord, error) {
	var (
		all      []ChangeRecord
		firstErr error
		ok       int
	)
	for _, r := range f.repos {
		recs, err := f.changesForRepo(ctx, r, since)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok++
		all = append(all, recs...)
	}
	if ok == 0 && firstErr != nil {
		return nil, firstErr
	}
	return all, nil
}

// changesForRepo mirrors the remote into the local cache (cloning on
// first use, fetching afterwards) and runs `git log` over the configured
// branch since the given time. Every record is stamped with the
// repository's service (explicit config or derived from the URL).
func (f *gitChangeFeed) changesForRepo(ctx context.Context, repo GitRepo, since time.Time) ([]ChangeRecord, error) {
	cachePath, err := f.ensureMirror(ctx, repo)
	if err != nil {
		return nil, err
	}

	service := repo.Service
	if service == "" {
		service = serviceFromURL(repo.URL)
	}

	authArgs, authEnv := gitAuthArgs(repo)
	args := append([]string{}, authArgs...)
	args = append(args, "-C", cachePath, "log")
	if repo.Branch != "" {
		args = append(args, repo.Branch)
	}
	args = append(args,
		"--no-color",
		"--since="+since.UTC().Format(time.RFC3339),
		// <RS>%H<US>%cI<US>%s — one line per commit.
		"--pretty=format:"+gitRecordSep+"%H"+gitFieldSep+"%cI"+gitFieldSep+"%s",
	)

	cmd := exec.CommandContext(ctx, "git", args...)
	if len(authEnv) > 0 {
		cmd.Env = append(os.Environ(), authEnv...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git log %q: %w: %s", repo.URL, err, strings.TrimSpace(stderr.String()))
	}

	return parseGitLog(stdout.String(), service), nil
}

// ensureMirror makes sure a local mirror of the remote exists under the
// cache directory and is up to date: it mirror-clones on first use and
// fetches on subsequent calls. Repo auth (HTTPS token / SSH key) is wired
// through for both operations. It returns the local mirror path.
func (f *gitChangeFeed) ensureMirror(ctx context.Context, repo GitRepo) (string, error) {
	path := f.cachePathFor(repo.URL)
	authArgs, authEnv := gitAuthArgs(repo)
	if _, err := os.Stat(path); err == nil {
		args := append([]string{}, authArgs...)
		args = append(args, "-C", path, "fetch", "--prune", "--quiet")
		cmd := exec.CommandContext(ctx, "git", args...)
		if len(authEnv) > 0 {
			cmd.Env = append(os.Environ(), authEnv...)
		}
		if out, ferr := cmd.CombinedOutput(); ferr != nil {
			return "", fmt.Errorf("git fetch %q: %w: %s", repo.URL, ferr, strings.TrimSpace(string(out)))
		}
		return path, nil
	}
	if err := os.MkdirAll(f.cacheDir, 0o700); err != nil {
		return "", fmt.Errorf("git cache dir %q: %w", f.cacheDir, err)
	}
	args := append([]string{}, authArgs...)
	args = append(args, "clone", "--mirror", "--quiet", repo.URL, path)
	cmd := exec.CommandContext(ctx, "git", args...)
	if len(authEnv) > 0 {
		cmd.Env = append(os.Environ(), authEnv...)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone %q: %w: %s", repo.URL, err, strings.TrimSpace(string(out)))
	}
	return path, nil
}

// gitAuthArgs returns the extra `git -c` arguments and environment
// additions needed to authenticate to repo's remote. An HTTPS token is
// injected via an http.extraHeader Authorization header (never persisted
// to the local mirror's config); an SSH key path is wired through
// GIT_SSH_COMMAND. An empty auth config returns nothing, so the ambient
// git credentials are used.
func gitAuthArgs(repo GitRepo) (configArgs []string, env []string) {
	if token := strings.TrimSpace(repo.Token); token != "" {
		// Basic auth with any username + token as password is accepted by
		// GitHub, GitLab, Bitbucket and most providers for HTTPS remotes.
		cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		configArgs = append(configArgs, "-c", "http.extraHeader=Authorization: Basic "+cred)
	}
	if key := strings.TrimSpace(repo.SSHKeyPath); key != "" {
		env = append(env, "GIT_SSH_COMMAND=ssh -i "+key+" -o IdentitiesOnly=yes")
	}
	return configArgs, env
}

// cachePathFor maps a remote URL to a deterministic local mirror path:
// the derived repository name plus a short hash of the URL keeps it both
// readable and collision-free across different remotes with the same
// basename.
func (f *gitChangeFeed) cachePathFor(url string) string {
	sum := sha256.Sum256([]byte(url))
	name := serviceFromURL(url)
	if name == "" {
		name = "repo"
	}
	return filepath.Join(f.cacheDir, sanitizeCacheName(name)+"-"+hex.EncodeToString(sum[:])[:12]+".git")
}

// sanitizeCacheName keeps a cache directory name filesystem-safe.
func sanitizeCacheName(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, name)
}

// parseGitLog turns the record-separated `git log` output into
// ChangeRecords, stamping each with the given service. Each record begins
// with gitRecordSep, followed by the SHA, committer date, and subject.
func parseGitLog(out, service string) []ChangeRecord {
	var records []ChangeRecord
	for _, block := range strings.Split(out, gitRecordSep) {
		block = strings.Trim(block, "\n")
		if block == "" {
			continue
		}
		line := block
		if i := strings.IndexByte(block, '\n'); i >= 0 {
			line = block[:i]
		}
		fields := strings.Split(line, gitFieldSep)
		if len(fields) != 3 {
			continue
		}
		ts, err := time.Parse(time.RFC3339, strings.TrimSpace(fields[1]))
		if err != nil {
			continue
		}
		records = append(records, ChangeRecord{
			Timestamp: ts.UTC(),
			Service:   service,
			Kind:      changesGitKind,
			Summary:   strings.TrimSpace(fields[2]),
			Ref:       shortSHA(fields[0]),
		})
	}
	return records
}

// serviceFromURL derives a service name from a git remote URL: the last
// path segment with any trailing ".git" stripped (e.g.
// "git@github.com:acme/web.git" → "web",
// "https://github.com/acme/api.git" → "api"). Empty when no segment can
// be derived.
func serviceFromURL(url string) string {
	u := strings.TrimSpace(url)
	u = strings.TrimRight(u, "/")
	u = strings.TrimSuffix(u, ".git")
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	return u
}

// shortSHA abbreviates a full commit hash to the conventional 7 chars.
func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
