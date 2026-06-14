package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/VersusControl/versus-incident/pkg/agent"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/controllers"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/metrics"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/routes"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/teams"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssmincidents"
	"github.com/go-redis/redis/v8"

	"github.com/VersusControl/versus-incident/pkg/common"

	"github.com/gofiber/fiber/v2"
)

func main() {
	err := c.LoadConfig("config/config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	cfg := c.GetConfig()

	// Construct the durable storage provider once and inject everywhere
	// that needs to persist (agent catalog/shadow, incident history). Today
	// only the file backend is implemented; redis/database return
	// storage.ErrUnsupported.
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
			Driver:       cfg.Storage.Database.Driver,
			DSN:          cfg.Storage.Database.DSN,
			MaxIncidents: cfg.Storage.Database.MaxIncidents,
		},
		Postgres: storage.PostgresOptions{
			DSN:          cfg.Storage.Postgres.DSN,
			MaxIncidents: cfg.Storage.Postgres.MaxIncidents,
		},
	})
	if err != nil {
		log.Fatalf("Failed to initialize storage: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Make storage available to the incident service (used to persist every
	// alert + record acks).
	services.SetStorage(store)

	// Initialize the operator-defined teams / members registry on the
	// same storage backend. Nil-tolerant: the controller responds with
	// 503 if construction failed, but a healthy boot wires it in.
	teamsStore, err := teams.NewStore(store)
	if err != nil {
		log.Printf("warn: teams store unavailable: %v", err)
		teamsStore = nil
	}

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true, // Disable the default Fiber banner
		// Golden rule #11: Fiber request strings (c.Get/Params/Query/…) are
		// backed by a pooled, reused request buffer. Several handlers persist
		// c.Params("id") (teams/members CRUD). Immutable copies those strings
		// off the buffer so they can safely outlive the request.
		Immutable: true,
	})

	app.Use(middleware.Logger())

	// Org-injection seam (X2-T3): stamps every request with a resolved
	// org id, defaulting to the single-tenant "default" org. Invisible to
	// OSS users; an external module registers a resolver to enable
	// multi-tenant scoping.
	app.Use(middleware.OrgInjector())

	routes.SetupRoutes(app, teamsStore)

	// Start queue listeners
	if cfg.Queue.Enable {
		listenerFactory := common.NewListenerFactory(cfg)
		listeners, err := listenerFactory.CreateListeners()
		if err != nil {
			log.Fatalf("Failed to create queue listeners: %v", err)
		}

		if cfg.Queue.SNS.Enable {
			app.Post(cfg.Queue.SNS.EndpointPath, controllers.SNS)
		}

		for _, listener := range listeners {
			go func(l core.QueueListener) {
				if err := l.StartListening(handleQueueMessage); err != nil {
					log.Printf("Listener error: %v", err)
				}
			}(listener)
		}
	}

	// Shared Redis client used by both on-call and the agent worker. We open
	// it once here so both subsystems share connections.
	var sharedRedis *redis.Client

	if cfg.OnCall.Enable || cfg.OnCall.InitializedOnly {
		redisOptions := handlerRedisOptions(cfg.Redis)

		// Initialize Redis client
		redisClient := redis.NewClient(redisOptions)

		// Test Redis connection
		if err := redisClient.Ping(context.Background()).Err(); err != nil {
			log.Fatal("Redis connection failed:", err)
		}
		sharedRedis = redisClient

		awsCfg, err := config.LoadDefaultConfig(context.Background())
		if err != nil {
			log.Fatal("Failed to load AWS config:", err)
		}

		awsClient := ssmincidents.NewFromConfig(awsCfg)
		core.InitOnCallWorkflow(awsClient, redisClient)
	}

	// Start the AI agent worker if enabled. Backwards compatible: when
	// agent.enable=false (the default) nothing changes.
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	if cfg.Agent.Enable {
		// Try to attach to the existing Redis client; if on-call wasn't
		// enabled but agent is, open one now (best effort — fall back to
		// in-memory cursors when Redis isn't reachable).
		rdb := sharedRedis
		if rdb == nil && cfg.Redis.Host != "" {
			rdb = redis.NewClient(handlerRedisOptions(cfg.Redis))
			if err := rdb.Ping(context.Background()).Err(); err != nil {
				log.Printf("agent: Redis unavailable (%v); cursors will be in-memory only", err)
				rdb = nil
			}
		}

		cat, err := startAgent(rootCtx, app, cfg.Agent, cfg.GatewaySecret, store, rdb)
		if err != nil {
			log.Fatalf("agent: failed to start: %v", err)
		}
		_ = cat // catalog handle held by goroutine + admin controller
	}

	// Mount the embedded UI LAST so it sits behind every API route. The
	// SPA fallback inside MountStaticUI defers /api/* and /healthz back
	// to the API handlers (which have already matched).
	controllers.MountStaticUI(app)

	// Trap SIGINT/SIGTERM so the HTTP server stops accepting connections
	// and any background workers (currently just the agent) get a chance to
	// flush state via rootCtx cancellation.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("received signal %s, shutting down…", sig)
		rootCancel()
		if err := app.Shutdown(); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}()

	addr := cfg.Host + ":" + strconv.Itoa(cfg.Port)

	printCustomBanner()
	if err := app.Listen(addr); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}

	rootCancel()
}

// startAgent constructs the worker, starts it in a goroutine, and registers
// admin routes on the fiber app. It returns the catalog so the caller can
// hold a reference (and so future hot-reload code has a handle to it).
func startAgent(ctx context.Context, app *fiber.App, cfg c.AgentConfig, gatewaySecret string, store storage.Provider, rdb *redis.Client) (*agent.Catalog, error) {
	catalog, err := agent.LoadCatalog(store)
	if err != nil {
		log.Printf("agent: catalog load warning: %v (starting fresh)", err)
	}
	log.Printf("agent: catalog loaded patterns=%d", catalog.Len())

	// Expose the live catalog size as a Prometheus gauge (scraped on demand).
	metrics.RegisterAgentPatternsGauge(func() float64 { return float64(catalog.Len()) })

	// Shadow log: only meaningful when running in shadow mode, but we always
	// load it so a mode switch (e.g. operator flips agent.mode=shadow at
	// runtime) doesn't lose history.
	shadowLog, err := agent.LoadShadowLog(store, 0)
	if err != nil {
		log.Printf("agent: shadow log load warning: %v (starting fresh)", err)
	}
	if cfg.Mode == "shadow" {
		log.Printf("agent: shadow log loaded events=%d", shadowLog.Len())
	}

	// Detect log: per-call audit trail (pattern + prompt + raw response
	// + finding) for the UI. Always loaded so a mode switch keeps history.
	detectLog, err := agent.LoadDetectLog(store, 0)
	if err != nil {
		log.Printf("agent: detect log load warning: %v (starting fresh)", err)
	}
	if cfg.Mode == "detect" {
		log.Printf("agent: detect log loaded events=%d", detectLog.Len())
	}

	miner := agent.NewMiner(cfg.Miner.SimilarityThreshold, cfg.Miner.TreeDepth, cfg.Miner.MaxChildren)
	for _, p := range catalog.All() {
		miner.AddCluster(p.ID, p.Template, p.Count)
	}

	redactor, redactErrs := agent.NewRedactor(cfg.Redaction.Enable && cfg.Redaction.RedactIPs, cfg.Redaction.ExtraPatterns)
	for _, e := range redactErrs {
		log.Printf("agent: redactor warning: %v", e)
	}

	matcher, regexErrs := agent.NewRegexMatcher(cfg.Regex)
	for _, e := range regexErrs {
		log.Printf("agent: regex warning: %v", e)
	}

	serviceMatcher, svcErrs := agent.NewServiceMatcher(cfg.ServicePatterns)
	for _, e := range svcErrs {
		log.Printf("agent: service_patterns warning: %v", e)
	}

	sources, sourceErrs := agent.BuildSources(cfg)
	for _, e := range sourceErrs {
		log.Printf("agent: source warning: %v", e)
	}
	if len(sources) == 0 {
		log.Printf("agent: no enabled sources configured; starting idle (admin endpoints active, no log polling)")
	}

	cursors := agent.NewCursorStore(rdb)

	aiBundle := agent.BuildAIs(cfg, catalog, store, nil)
	if aiBundle.Detect != nil {
		log.Printf("agent: AI SRE enabled provider=%s model=%s rate_limit=%d/hr",
			aiBundle.Detect.Name(), cfg.AI.Model, cfg.AI.MaxCallsPerHour)
	}
	if aiBundle.Analyze != nil {
		services.SetAnalyzeAgent(aiBundle.Analyze)
	}

	worker, err := agent.NewWorker(agent.WorkerOptions{
		Cfg:      cfg,
		Sources:  sources,
		Cursors:  cursors,
		Redactor: redactor,
		Matcher:  matcher,
		Miner:    miner,
		Catalog:  catalog,
		Shadow:   shadowLog,
		Detect:   detectLog,
		Services: serviceMatcher,
		AI:       aiBundle,
		Emitter:  services.CreateIncidentFromFinding,
	})
	if err != nil {
		return nil, err
	}

	go worker.Run(ctx)

	// Admin endpoints (require GATEWAY_SECRET).
	if gatewaySecret == "" {
		return nil, fmt.Errorf("agent: gateway_secret is not configured — /api/agent/* admin endpoints require a secret")
	}
	api := app.Group("/api")
	controllers.NewAgentController(catalog, shadowLog, detectLog, aiBundle.Runbooks != nil).Register(api)
	controllers.NewRunbookAdminController(aiBundle.Runbooks).Register(api)

	return catalog, nil
}

func printCustomBanner() {
	cfg := c.GetConfig()

	// Prefer the operator-supplied public_host (e.g. https://versus.example.com)
	// for the URLs we surface. Fall back to host:port for plain local dev.
	base := strings.TrimRight(cfg.PublicHost, "/")
	if base == "" {
		base = fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port)
	}

	log.Printf(`

V       V   EEEEE   RRRRR   SSSSS   U       U   SSSSS
V       V   E       R   R   S       U       U   S    
V       V   EEEEE   RRRRR   SSSSS   U       U   SSSSS
 V V V V    E       R  R         S  U       U        S
   V V      EEEEE   R   R   SSSSS    UUUUUUU    SSSSS

┌───────────────────────────────────────────────────┐
│                Versus Incident v1                 │
│       (bound on host %s and port %d)       │
└───────────────────────────────────────────────────┘

Dashboard UI   -> %s/
Health check   -> %s/healthz
Create alert   -> POST %s/api/incidents
Acknowledge    -> GET  %s/api/ack/:id
Admin incidents-> GET  %s/api/admin/incidents
Agent status   -> GET  %s/api/agent/status
`, cfg.Host, cfg.Port,
		base, base, base, base, base, base)
}

func handleQueueMessage(content *map[string]interface{}) error {
	// Stamp the ingress transport so persisted incidents say "sqs"
	// instead of the default "http". Agent-originated incidents carry
	// their own Source and ignore this hint.
	overwrite := map[string]string{"incident_source": "sqs"}
	return services.CreateIncident("", content, &overwrite) // teamID as empty string
}

func handlerRedisOptions(rc c.RedisConfig) *redis.Options {
	redisOptions := &redis.Options{
		Addr:     rc.Host + ":" + strconv.Itoa(rc.Port),
		Password: rc.Password,
		DB:       rc.DB,
	}

	if rc.InsecureSkipVerify {
		// Configure TLS
		redisOptions.TLSConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	} else {
		// Load system CA pool by default
		rootCAs, _ := x509.SystemCertPool()
		if rootCAs == nil {
			rootCAs = x509.NewCertPool()
		}

		// Add custom CA if provided (optional)
		if caCertPath := os.Getenv("REDIS_CA_CERT"); caCertPath != "" {
			caCert, err := os.ReadFile(caCertPath)
			if err != nil {
				log.Fatal("Failed to read CA cert:", err)
			}
			if ok := rootCAs.AppendCertsFromPEM(caCert); !ok {
				log.Fatal("Failed to append CA cert")
			}
		}

		// Configure TLS
		redisOptions.TLSConfig = &tls.Config{
			RootCAs:    rootCAs,
			MinVersion: tls.VersionTLS12, // Enforce modern TLS
		}
	}

	return redisOptions
}
