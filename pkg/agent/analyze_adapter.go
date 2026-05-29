package agent

import (
	analyzetools "github.com/VersusControl/versus-incident/pkg/agent/ai/analyze/tools"
)

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
