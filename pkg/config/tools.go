package config

// -----------------------------------------------------------------------------
// Per-tool configuration (tools.yaml)
// -----------------------------------------------------------------------------
//
// ToolsConfig is the home for every analyze-mode tool that needs external
// data configuration. It is loaded from the optional sibling file
// `tools.yaml` (same directory as the main config), mirroring the
// `agent_sources.yaml` pattern. A missing file leaves every section zero,
// which degrades the affected tool to a clean "nothing found" (or leaves
// it unregistered) rather than an error.
//
// This is per-tool DATA config, never a registration allow-list: tools
// are still registered in code via `analyzetools.Default`.

// ToolsConfig groups the configurable analyze-mode tools by tool name.
// It also carries the shared tool-loop knobs (tool_timeout,
// parallel_tools) that apply to every analyze tool dispatch.
type ToolsConfig struct {
	// ToolTimeout caps how long a single tool dispatch may run before it
	// is abandoned, e.g. "20s". A timeout surfaces as a tool error in the
	// audit trace (not a hard failure) so one slow tool cannot consume
	// the whole analysis budget. Empty, "0", or an unparseable value
	// inherits the built-in default (20s).
	ToolTimeout string `mapstructure:"tool_timeout"`
	// ParallelTools controls whether multiple tool calls emitted in one
	// model turn run concurrently. Off by default: tool dispatch is
	// sequential, which keeps load on downstream sources predictable. The
	// per-call audit trace is ordered deterministically regardless.
	ParallelTools bool `mapstructure:"parallel_tools"`
	// RecentChanges configures the `recent_changes` tool.
	RecentChanges RecentChangesToolConfig `mapstructure:"recent_changes"`
	// DescribeDependencies configures the `describe_dependencies` tool.
	DescribeDependencies DescribeDependenciesToolConfig `mapstructure:"describe_dependencies"`
}

// DescribeDependenciesToolConfig configures the `describe_dependencies`
// tool. It carries the optional service-dependency graph; an empty
// `Services` list leaves the tool unregistered.
type DescribeDependenciesToolConfig struct {
	// Services is the operator-authored service-dependency graph. Each
	// entry has a `name` and a `depends_on` list of upstream services;
	// the reverse (downstream) edges are derived automatically at build
	// time. Empty leaves the `describe_dependencies` tool unregistered.
	Services []ServiceDependency `mapstructure:"services"`
}

// ServiceDependency is one node in the optional service-dependency graph.
// `DependsOn` lists the upstream services this service relies on; the
// reverse (downstream) edges are derived automatically.
type ServiceDependency struct {
	Name      string   `mapstructure:"name"`
	DependsOn []string `mapstructure:"depends_on"`
}

// RecentChangesToolConfig configures the `recent_changes` tool. Today the
// only supported change source is one or more remote git repositories.
type RecentChangesToolConfig struct {
	// Git points the tool at a set of remote git repositories' commit
	// histories. An empty Repos list leaves the tool unregistered.
	Git RecentChangesGitConfig `mapstructure:"git"`
}

// RecentChangesGitConfig points the `recent_changes` tool at one or more
// remote git repositories. The tool shells out to the `git` binary (which
// must be on PATH), so no Go git dependency is pulled in and no separate
// event pipeline is required: the deploy/change record is the commit log
// the team already keeps. Each repository is mirror-cloned into a local
// cache on first use and fetched on subsequent lookups.
//
// Example (tools.yaml):
//
//	tools:
//	  recent_changes:
//	    git:
//	      auth:                                      # global default auth
//	        token: ${GIT_TOKEN}                      # HTTPS token / PAT
//	        ssh_key_path: /home/versus/.ssh/id_ed25519
//	      repos:
//	        - url: https://github.com/acme/api.git   # remote clone URL
//	          branch: main                           # optional; empty = default HEAD
//	          service: api                           # optional; empty = derived from URL
//	        - url: git@github.com:acme/web.git        # service auto-detected as "web"
//	          auth:                                   # optional; overrides the global auth
//	            ssh_key_path: /home/versus/.ssh/web_deploy
//
// An empty Repos list leaves the `recent_changes` tool unregistered.
type RecentChangesGitConfig struct {
	// Auth is the global default authentication applied to every repo that
	// does not define its own. Empty means rely on the ambient git
	// credentials (credential helper / default SSH keys).
	Auth GitAuthConfig `mapstructure:"auth"`
	// Repos is the set of remote git repositories to read commits from.
	// Empty leaves the `recent_changes` tool unregistered.
	Repos []RecentChangesGitRepo `mapstructure:"repos"`
}

// GitAuthConfig holds the credentials used to authenticate to a remote
// git repository. Both fields are optional; an empty config relies on the
// ambient git credentials (credential helper / default SSH keys).
type GitAuthConfig struct {
	// Token is an HTTPS access token / personal access token used for
	// https:// remotes (sent via an Authorization header, never persisted
	// to the local mirror's config).
	Token string `mapstructure:"token"`
	// SSHKeyPath is the path to a private SSH key used for ssh:// or
	// scp-like (git@host:org/repo) remotes.
	SSHKeyPath string `mapstructure:"ssh_key_path"`
}

// RecentChangesGitRepo is one remote git repository the change feed reads.
type RecentChangesGitRepo struct {
	// URL is the remote clone URL (https or scp-like git@host:org/repo).
	// Empty entries are ignored.
	URL string `mapstructure:"url"`
	// Branch optionally pins which branch to read. Empty reads the
	// repository's default HEAD.
	Branch string `mapstructure:"branch"`
	// Service maps every commit in this repository to a service name. When
	// empty the service is auto-detected from the repository name in the
	// URL (e.g. git@github.com:acme/web.git → "web").
	Service string `mapstructure:"service"`
	// Auth optionally overrides the global git auth for this repo. Empty
	// fields fall back to the global default in RecentChangesGitConfig.
	Auth GitAuthConfig `mapstructure:"auth"`
}
