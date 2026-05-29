package services

import (
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// store holds the process-wide storage provider used to persist
// incidents. Set once at startup by main.SetStorage. Nil-safe: callers
// MUST guard against a nil store so unit tests that don't wire one in
// still work.
var store storage.Provider

// SetStorage installs the storage provider used to persist incidents and
// record acks. Called once from main after config load.
func SetStorage(p storage.Provider) { store = p }

// Storage returns the currently installed storage provider, or nil when
// the process is running without persistence (older tests).
func Storage() storage.Provider { return store }

// analyzeAgent holds the process-wide analyze-kind AI agent. Set once
// at startup by main when agent.ai.enable is true. Nil-safe: the admin
// controller returns 503 when this is nil.
var analyzeAgent core.AIAgent

// SetAnalyzeAgent installs the analyze agent used by the admin
// /analyze endpoint. Pass nil to disable analyze.
func SetAnalyzeAgent(a core.AIAgent) { analyzeAgent = a }

// AnalyzeAgent returns the installed analyze agent or nil.
func AnalyzeAgent() core.AIAgent { return analyzeAgent }
