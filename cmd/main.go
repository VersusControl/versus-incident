package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log"
	"os"
	"strconv"

	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/controllers"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/routes"
	"github.com/VersusControl/versus-incident/pkg/services"
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

	app := fiber.New(fiber.Config{
		DisableStartupMessage: true, // Disable the default Fiber banner
	})

	app.Use(middleware.Logger())

	routes.SetupRoutes(app)

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

	if cfg.OnCall.Enable || cfg.OnCall.InitializedOnly {
		redisOptions := handlerRedisOptions(cfg.Redis)

		// Initialize Redis client
		redisClient := redis.NewClient(redisOptions)

		// Test Redis connection
		if err := redisClient.Ping(context.Background()).Err(); err != nil {
			log.Fatal("Redis connection failed:", err)
		}

		awsCfg, err := config.LoadDefaultConfig(context.Background())
		if err != nil {
			log.Fatal("Failed to load AWS config:", err)
		}

		awsClient := ssmincidents.NewFromConfig(awsCfg)
		core.InitOnCallWorkflow(awsClient, redisClient)
	}

	addr := cfg.Host + ":" + strconv.Itoa(cfg.Port)

	printCustomBanner()
	if err := app.Listen(addr); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func printCustomBanner() {
	cfg := c.GetConfig()

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

/api/incidents -> receive incident data
/api%s       -> receive alerts from AWS SNS
/api/ack       -> acknowledge on-call alerts
`, cfg.Host, cfg.Port, cfg.Queue.SNS.EndpointPath)
}

func handleQueueMessage(content *map[string]interface{}) error {
	return services.CreateIncident("", content) // teamID as empty string
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
