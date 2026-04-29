package agent

import (
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
)

// BuildSources constructs every enabled SignalSource from AgentConfig. Sources
// with type set to an unknown value are skipped (with a non-fatal error in
// the returned slice) so a config typo cannot stop the agent from starting.
func BuildSources(cfg config.AgentConfig) ([]core.SignalSource, []error) {
	var sources []core.SignalSource
	var errs []error
	for _, s := range cfg.Sources {
		if !s.Enable {
			continue
		}
		switch s.Type {
		case "elasticsearch":
			es, err := signalsources.NewElasticsearchSource(s.Name, s.Elasticsearch)
			if err != nil {
				errs = append(errs, fmt.Errorf("source %s: %w", s.Name, err))
				continue
			}
			sources = append(sources, es)
		case "file":
			fs, err := signalsources.NewFileSource(s.Name, s.File)
			if err != nil {
				errs = append(errs, fmt.Errorf("source %s: %w", s.Name, err))
				continue
			}
			sources = append(sources, fs)
		default:
			errs = append(errs, fmt.Errorf("source %s: unknown type %q", s.Name, s.Type))
		}
	}
	return sources, errs
}
