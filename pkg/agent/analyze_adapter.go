package agent

import (
	"context"
	"fmt"
	"time"

	analyzetools "github.com/VersusControl/versus-incident/pkg/agent/ai/analyze/tools"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// signalReaderAdapter wraps a set of core.SignalSource instances so they
// satisfy analyzetools.SignalReader without leaking pkg/agent (or the
// concrete source types) into the tools package. The wrapped sources are
// an independent set built solely for the read-only get_related_logs
// tool, so calling Pull here never advances the worker's cursors.
type signalReaderAdapter struct {
	sources map[string]core.SignalSource
	order   []string
}

func newSignalReaderAdapter(sources []core.SignalSource) analyzetools.SignalReader {
	if len(sources) == 0 {
		return nil
	}
	m := make(map[string]core.SignalSource, len(sources))
	order := make([]string, 0, len(sources))
	for _, s := range sources {
		if s == nil {
			continue
		}
		name := s.Name()
		if _, dup := m[name]; dup {
			continue
		}
		m[name] = s
		order = append(order, name)
	}
	if len(m) == 0 {
		return nil
	}
	return &signalReaderAdapter{sources: m, order: order}
}

func (a *signalReaderAdapter) Sources() []string {
	if a == nil {
		return nil
	}
	return append([]string(nil), a.order...)
}

func (a *signalReaderAdapter) Pull(ctx context.Context, source string, since time.Time) ([]core.Signal, error) {
	if a == nil {
		return nil, fmt.Errorf("signal reader not configured")
	}
	src, ok := a.sources[source]
	if !ok {
		return nil, fmt.Errorf("unknown source %q", source)
	}
	sigs, _, err := src.Pull(ctx, since)
	return sigs, err
}

// catalogAdapter wraps *Catalog so it satisfies the
// analyzetools.PatternCatalog interface without leaking the agent
// package into the tools package. This keeps the import graph
// one-way: pkg/agent -> tools.
type catalogAdapter struct{ c *Catalog }

func newCatalogAdapter(c *Catalog) analyzetools.PatternCatalog {
	if c == nil {
		return nil
	}
	return &catalogAdapter{c: c}
}

func (a *catalogAdapter) Get(id string) *analyzetools.PatternView {
	if a == nil || a.c == nil {
		return nil
	}
	p := a.c.Get(id)
	if p == nil {
		return nil
	}
	v := toView(p)
	return &v
}

func (a *catalogAdapter) All() []*analyzetools.PatternView {
	if a == nil || a.c == nil {
		return nil
	}
	all := a.c.All()
	out := make([]*analyzetools.PatternView, 0, len(all))
	for _, p := range all {
		v := toView(p)
		out = append(out, &v)
	}
	return out
}

func (a *catalogAdapter) AllServices() map[string]analyzetools.ServiceInfo {
	if a == nil || a.c == nil {
		return nil
	}
	src := a.c.AllServices()
	out := make(map[string]analyzetools.ServiceInfo, len(src))
	for k, v := range src {
		out[k] = analyzetools.ServiceInfo{FirstSeen: v.FirstSeen}
	}
	return out
}

func toView(p *Pattern) analyzetools.PatternView {
	tags := append([]string(nil), p.Tags...)
	return analyzetools.PatternView{
		ID:        p.ID,
		Template:  p.Template,
		Source:    p.Source,
		Service:   p.Service,
		RuleName:  p.RuleName,
		Verdict:   p.Verdict,
		Tags:      tags,
		Count:     p.Count,
		Baseline:  p.BaselineFrequency,
		FirstSeen: p.FirstSeen,
		LastSeen:  p.LastSeen,
	}
}

// buildDependencyGraph converts the operator-authored config service
// graph into the tools-package DependencyGraph used by the
// describe_dependencies tool. A nil/empty input yields a nil graph so
// the tool is omitted by analyzetools.Default.
func buildDependencyGraph(nodes []config.ServiceDependency) *analyzetools.DependencyGraph {
	if len(nodes) == 0 {
		return nil
	}
	dependsOn := make(map[string][]string, len(nodes))
	for _, n := range nodes {
		if n.Name == "" {
			continue
		}
		dependsOn[n.Name] = append(dependsOn[n.Name], n.DependsOn...)
	}
	if len(dependsOn) == 0 {
		return nil
	}
	return analyzetools.NewDependencyGraph(dependsOn)
}

// buildGitRepos converts the operator-authored config repo list into the
// tools-package GitRepo slice used by the recent_changes change feed.
// Each repo's auth falls back to the global default (git.auth) when its
// own auth fields are empty.
func buildGitRepos(git config.RecentChangesGitConfig) []analyzetools.GitRepo {
	if len(git.Repos) == 0 {
		return nil
	}
	out := make([]analyzetools.GitRepo, 0, len(git.Repos))
	for _, r := range git.Repos {
		token := r.Auth.Token
		if token == "" {
			token = git.Auth.Token
		}
		sshKey := r.Auth.SSHKeyPath
		if sshKey == "" {
			sshKey = git.Auth.SSHKeyPath
		}
		out = append(out, analyzetools.GitRepo{
			URL:        r.URL,
			Branch:     r.Branch,
			Service:    r.Service,
			Token:      token,
			SSHKeyPath: sshKey,
		})
	}
	return out
}
