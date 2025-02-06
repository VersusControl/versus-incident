package main

import (
	"log"
	"strconv"
	"versus-incident/pkg/common"
	"versus-incident/pkg/core"
	"versus-incident/pkg/middleware"
	"versus-incident/pkg/routes"
	"versus-incident/pkg/services"

	"github.com/gofiber/fiber/v2"
)

func main() {
	err := common.LoadConfig("config/config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	cfg := common.GetConfig()

	app := fiber.New()

	app.Use(middleware.Logger())

	routes.SetupRoutes(app)

	// Start queue listeners
	if cfg.Queue.Enable {
		listenerFactory := common.NewListenerFactory(cfg)
		listeners, err := listenerFactory.CreateListeners()
		if err != nil {
			log.Fatalf("Failed to create queue listeners: %v", err)
		}

		for _, listener := range listeners {
			go func(l core.QueueListener) {
				if err := l.StartListening(handleQueueMessage); err != nil {
					log.Printf("Listener error: %v", err)
				}
			}(listener)
		}
	}

	addr := cfg.Host + ":" + strconv.Itoa(cfg.Port)

	if err := app.Listen(addr); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func handleQueueMessage(content map[string]interface{}) error {
	return services.CreateIncident("", content) // teamID as empty string
}
