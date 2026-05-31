package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"

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

// gitChangeFeed reads change records from one or more remote git
// repositories' commit histories on every call. It uses the go-git
// library (pure Go) so no external `git` binary is required. Each
// repository is mirror-cloned into a local cache directory on first use
// and fetched on subsequent lookups; reading per-call keeps the feed
// fresh and the tool stateless.
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
	// remote. Empty relies on ambient credentials.
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
// recent_changes tool. No external git binary is required.
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

// changesForRepo ensures the mirror exists, fetches the latest, and
// walks the commit log from the configured branch since the given time.
func (f *gitChangeFeed) changesForRepo(_ context.Context, repo GitRepo, since time.Time) ([]ChangeRecord, error) {
	r, err := f.ensureMirror(repo)
	if err != nil {
		return nil, err
	}

	service := repo.Service
	if service == "" {
		service = serviceFromURL(repo.URL)
	}

	// Resolve branch reference.
	var ref *plumbing.Reference
	if repo.Branch != "" {
		ref, err = r.Reference(plumbing.NewBranchReferenceName(repo.Branch), true)
		if err != nil {
			// Try remote ref naming (mirrors store refs/heads/*)
			ref, err = r.Reference(plumbing.ReferenceName("refs/heads/"+repo.Branch), true)
			if err != nil {
				return nil, fmt.Errorf("resolve branch %q in %q: %w", repo.Branch, repo.URL, err)
			}
		}
	} else {
		head, herr := r.Head()
		if herr != nil {
			// Bare/mirror repos may not have HEAD; try "main" then "master".
			for _, fallback := range []string{"refs/heads/main", "refs/heads/master"} {
				ref, err = r.Reference(plumbing.ReferenceName(fallback), true)
				if err == nil {
					break
				}
			}
			if ref == nil {
				return nil, fmt.Errorf("resolve HEAD in %q: %w", repo.URL, herr)
			}
		} else {
			ref = head
		}
	}

	commitIter, err := r.Log(&git.LogOptions{
		From:  ref.Hash(),
		Order: git.LogOrderCommitterTime,
		Since: &since,
	})
	if err != nil {
		return nil, fmt.Errorf("log %q: %w", repo.URL, err)
	}

	var records []ChangeRecord
	err = commitIter.ForEach(func(c *object.Commit) error {
		records = append(records, ChangeRecord{
			Timestamp: c.Committer.When.UTC(),
			Service:   service,
			Kind:      changesGitKind,
			Summary:   firstLine(c.Message),
			Ref:       shortSHA(c.Hash.String()),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("iterate commits %q: %w", repo.URL, err)
	}

	return records, nil
}

// ensureMirror makes sure a local bare clone of the remote exists under
// the cache directory and is up to date: it clones on first use and
// fetches on subsequent calls. Returns the opened *git.Repository.
func (f *gitChangeFeed) ensureMirror(repo GitRepo) (*git.Repository, error) {
	path := f.cachePathFor(repo.URL)
	auth := gitAuth(repo)

	if _, err := os.Stat(path); err == nil {
		// Already cloned — open and fetch.
		r, oerr := git.PlainOpen(path)
		if oerr != nil {
			return nil, fmt.Errorf("open mirror %q: %w", repo.URL, oerr)
		}
		remote, rerr := r.Remote("origin")
		if rerr != nil {
			return nil, fmt.Errorf("remote origin %q: %w", repo.URL, rerr)
		}
		ferr := remote.Fetch(&git.FetchOptions{
			Auth:  auth,
			Force: true,
			Tags:  git.NoTags,
		})
		if ferr != nil && ferr != git.NoErrAlreadyUpToDate {
			return nil, fmt.Errorf("fetch %q: %w", repo.URL, ferr)
		}
		return r, nil
	}

	// First use — bare clone.
	if err := os.MkdirAll(f.cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("git cache dir %q: %w", f.cacheDir, err)
	}
	r, err := git.PlainClone(path, true, &git.CloneOptions{
		URL:  repo.URL,
		Auth: auth,
		Tags: git.NoTags,
	})
	if err != nil {
		return nil, fmt.Errorf("clone %q: %w", repo.URL, err)
	}
	return r, nil
}

// gitAuth returns the transport.AuthMethod for the given repo config.
// Token → HTTP Basic auth. SSHKeyPath → SSH public-key auth. Both empty
// → nil (ambient credentials).
func gitAuth(repo GitRepo) transport.AuthMethod {
	if token := strings.TrimSpace(repo.Token); token != "" {
		return &http.BasicAuth{
			Username: "x-access-token",
			Password: token,
		}
	}
	if key := strings.TrimSpace(repo.SSHKeyPath); key != "" {
		// Read the key file; on error fall through to nil (ambient).
		publicKeys, err := ssh.NewPublicKeysFromFile("git", key, "")
		if err == nil {
			return publicKeys
		}
	}
	return nil
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

// firstLine extracts the first line (commit subject) from a message.
func firstLine(msg string) string {
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		return strings.TrimSpace(msg[:i])
	}
	return strings.TrimSpace(msg)
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
