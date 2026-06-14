// Package metrics exposes a Prometheus registry and the /metrics HTTP
// handler. It is a leaf package — it imports only client_golang, never any
// versus package — so any subsystem can import it to record a metric
// without creating an import cycle. Other packages increment the exported
// collectors; cmd wires dynamic gauges via RegisterAgentPatternsGauge.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// registry is a private registry (not the global default) so tests and
// embedders get a clean, predictable metric set.
var registry = prometheus.NewRegistry()

// IncidentsTotal counts incidents created, labelled by notification
// delivery status ("sent" | "partial" | "failed"). Incremented by the
// incident service after fan-out.
var IncidentsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "versus",
	Name:      "incidents_total",
	Help:      "Incidents created, by notification delivery status.",
}, []string{"status"})

func init() {
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		IncidentsTotal,
	)
}

// Registry returns the underlying registry so embedders can register extra
// collectors.
func Registry() *prometheus.Registry { return registry }

// Handler returns the Prometheus exposition handler for the /metrics route.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

// RegisterAgentPatternsGauge registers a gauge reporting the current agent
// catalog size via the supplied callback. Call once at agent startup; the
// callback is invoked on every scrape, so it must be cheap and concurrency
// safe (Catalog.Len holds a read lock).
func RegisterAgentPatternsGauge(count func() float64) {
	registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: "versus",
		Subsystem: "agent",
		Name:      "patterns",
		Help:      "Patterns currently held in the agent catalog.",
	}, count))
}
