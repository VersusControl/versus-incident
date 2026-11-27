package services

import (
	"sync"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/incidentcounts"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// store holds the process-wide storage provider used to persist
// incidents. Set once at startup by main.SetStorage. Nil-safe: callers
// MUST guard against a nil store so unit tests that don't wire one in
// still work.
var store storage.Provider

// incidentCounts caches incident status counts for the installed store. It
// belongs to that store: installing another one starts an empty cache, so a
// count from one store is never served for another.
var (
	incidentCountsMu sync.Mutex
	incidentCounts   = incidentcounts.New(incidentcounts.Options{})
)

// SetStorage installs the storage provider used to persist incidents and
// record acks. Called once from main after config load.
func SetStorage(p storage.Provider) {
	store = p
	resetCountSettings()
	incidentCountsMu.Lock()
	incidentCounts = incidentcounts.New(incidentcounts.Options{})
	incidentCountsMu.Unlock()
}

// Storage returns the currently installed storage provider, or nil when
// the process is running without persistence (older tests).
func Storage() storage.Provider { return store }

// IncidentCounts returns the incident count cache for the installed store.
func IncidentCounts() *incidentcounts.Cache {
	incidentCountsMu.Lock()
	defer incidentCountsMu.Unlock()
	return incidentCounts
}

// SetIncidentCounts installs c as the incident count cache, so a test can
// give it its own clock. Call it after SetStorage, which installs a new one.
func SetIncidentCounts(c *incidentcounts.Cache) {
	incidentCountsMu.Lock()
	incidentCounts = c
	incidentCountsMu.Unlock()
}

// MarkIncidentCountsStale records that an operator just changed an
// incident's status, so the next count request recounts.
func MarkIncidentCountsStale() { IncidentCounts().MarkStale() }

// analyzeAgent holds the process-wide analyze-kind AI agent. Set once
// at startup by main when agent.ai.enable is true. Nil-safe: the admin
// controller returns 503 when this is nil.
var analyzeAgent core.AIAgent

// SetAnalyzeAgent installs the analyze agent used by the admin
// /analyze endpoint. Pass nil to disable analyze.
func SetAnalyzeAgent(a core.AIAgent) { analyzeAgent = a }

// AnalyzeAgent returns the installed analyze agent or nil.
func AnalyzeAgent() core.AIAgent { return analyzeAgent }
