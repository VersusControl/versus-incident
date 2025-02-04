package main

import (
	"log"
	"strconv"
	"versus-incident/pkg/common"
	"versus-incident/pkg/middleware"
	"versus-incident/pkg/routes"

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

	addr := cfg.Host + ":" + strconv.Itoa(cfg.Port)

	if err := app.Listen(addr); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
