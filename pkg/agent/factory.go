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
		case "loki":
			lk, err := signalsources.NewLokiSource(s.Name, s.Loki)
			if err != nil {
				errs = append(errs, fmt.Errorf("source %s: %w", s.Name, err))
				continue
			}
			sources = append(sources, lk)
		case "cloudwatchlogs":
			cw, err := signalsources.NewCloudWatchLogsSource(s.Name, s.CloudWatchLogs)
			if err != nil {
				errs = append(errs, fmt.Errorf("source %s: %w", s.Name, err))
				continue
			}
			sources = append(sources, cw)
		case "graylog":
			gl, err := signalsources.NewGraylogSource(s.Name, s.Graylog)
			if err != nil {
				errs = append(errs, fmt.Errorf("source %s: %w", s.Name, err))
				continue
			}
			sources = append(sources, gl)
		case "splunk":
			sp, err := signalsources.NewSplunkSource(s.Name, s.Splunk)
			if err != nil {
				errs = append(errs, fmt.Errorf("source %s: %w", s.Name, err))
				continue
			}
			sources = append(sources, sp)
		default:
			// Source types not built into OSS are resolved through the
			// registration hook (signalsources.Register). The enterprise
			// module registers its metric/trace data sources from an init(),
			// so they wire up here without OSS importing enterprise.
			factory, ok := signalsources.Lookup(s.Type)
			if !ok {
				if signalsources.RequiresEnterprise(s.Type) {
					errs = append(errs, fmt.Errorf("source %s: %w", s.Name, signalsources.ErrRequiresEnterprise(s.Type)))
				} else {
					errs = append(errs, fmt.Errorf("source %s: unknown type %q", s.Name, s.Type))
				}
				continue
			}
			src, err := factory(s.Name, s.Options)
			if err != nil {
				errs = append(errs, fmt.Errorf("source %s: %w", s.Name, err))
				continue
			}
			sources = append(sources, src)
		}
	}
	return sources, errs
}
