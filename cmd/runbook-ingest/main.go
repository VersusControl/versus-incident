// Command runbook-ingest builds the runbook-RAG corpus for the SRE
// agent's read-only find_runbook tool. It scans the runbook source
// directory of Markdown runbooks, embeds each through the configured
// OpenAI-compatible embeddings endpoint, and persists the vectors to the
// same storage backend the server reads at boot.
//
// You usually do NOT need to run this: when tools.find_runbook.embedding_model
// is set, the server auto-ingests the runbook source directory at boot
// (incrementally — only new or edited runbooks are embedded). This command
// exists for pre-baking the corpus out-of-band, e.g. in CI or an air-gapped
// image build, so the server starts with the corpus already populated.
//
// This is the WRITE path: it lives in its own command so the corpus can be
// built without a running server, and the analyze tool catalog stays
// read-only.
//
// Usage:
//
//	runbook-ingest [-config config/config.yaml] [-org ORG]
//
// Place your *.md runbooks in the data folder under runbooks/ (e.g.
// ./data/runbooks; /app/data/runbooks in the container image). The embedding
// model comes from tools.find_runbook.embedding_model and the API
// credential is the shared agent.ai.api_key. The embedding backend is
// selected by agent.ai.provider (openai / ollama / gemini), so pointing
// provider at a local Ollama keeps embeddings inside the operator's own
// network.
package main

import (
	"context"
	"flag"
	"log"
	"path/filepath"

	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/runbook"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func main() {
	configPath := flag.String("config", "config/config.yaml", "path to config.yaml")
	orgFlag := flag.String("org", "", "org id to scope ingested runbooks (default: \"default\")")
	flag.Parse()

	if err := c.LoadConfig(*configPath); err != nil {
		log.Fatalf("runbook-ingest: load config: %v", err)
	}
	cfg := c.GetConfig()

	fr := cfg.Agent.Tools.FindRunbook
	if fr.EmbeddingModel == "" {
		log.Fatalf("runbook-ingest: tools.find_runbook.embedding_model is not configured")
	}

	// Runbooks are read from the data folder under runbooks/ (e.g.
	// ./data/runbooks; /app/data/runbooks in the container image).
	dir := filepath.Join(storage.DefaultDataDir, runbook.SourceSubdir)

	store, err := storage.New(storage.Config{
		Type: cfg.Storage.Type,
		File: storage.FileOptions{
			MaxIncidents: cfg.Storage.File.MaxIncidents,
		},
		Redis: storage.RedisOptions{
			Host:               cfg.Storage.Redis.Host,
			Port:               cfg.Storage.Redis.Port,
			Password:           cfg.Storage.Redis.Password,
			DB:                 cfg.Storage.Redis.DB,
			InsecureSkipVerify: cfg.Storage.Redis.InsecureSkipVerify,
			KeyPrefix:          cfg.Storage.Redis.KeyPrefix,
			MaxIncidents:       cfg.Storage.Redis.MaxIncidents,
		},
		Database: storage.DatabaseOptions{
			Driver: cfg.Storage.Database.Driver,
			DSN:    cfg.Storage.Database.DSN,
		},
		Postgres: storage.PostgresOptions{
			DSN: cfg.Storage.Postgres.DSN,
		},
	})
	if err != nil {
		log.Fatalf("runbook-ingest: init storage: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	embedder, err := einowrap.NewEmbedder(ctx, c.AgentAIConfig{
		Provider: cfg.Agent.AI.Provider,
		Model:    fr.EmbeddingModel,
		APIKey:   cfg.Agent.AI.APIKey,
	}, einowrap.Options{})
	if err != nil {
		log.Fatalf("runbook-ingest: build embedder: %v", err)
	}

	rbStore, err := runbook.LoadStore(store)
	if err != nil {
		log.Fatalf("runbook-ingest: load corpus: %v", err)
	}

	n, err := runbook.IngestDir(ctx, rbStore, embedder, dir, *orgFlag)
	if err != nil {
		log.Fatalf("runbook-ingest: ingest %q: %v", dir, err)
	}
	log.Printf("runbook-ingest: ingested %d runbook(s) from %q into corpus (%d total)", n, dir, rbStore.Len())
}
