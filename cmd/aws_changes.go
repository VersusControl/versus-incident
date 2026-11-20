package main

import (
	"log"

	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/aws/harnessendpoint"
	awsmount "github.com/VersusControl/versus-incident/pkg/aws/mount"
	c "github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// mountAWS mounts the AWS connector, Changes and FinOps for the open-source
// build: the default org, the 24 h Changes limits and every licensed FinOps
// option locked. The AWS transport is the hardened default except in a
// harness_awsendpoint test build.
func mountAWS(app *fiber.App, store storage.Provider, cfg *c.Config) *awsmount.Mounted {
	mounted, err := awsmount.Mount(awsmount.Deps{
		Store:      store,
		Scope:      tenancy.DefaultOrgScope(),
		Config:     cfg,
		Router:     app.Group("/api"),
		HTTPClient: harnessendpoint.HTTPClient(),
	})
	if err != nil {
		log.Fatalf("aws cost: invalid options")
	}
	return mounted
}
