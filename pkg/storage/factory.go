package storage

import (
	"fmt"
	"os"
)

// Config mirrors the root-level `storage:` block in config.yaml. It is
// kept here (rather than in pkg/config) so tests in this package don't
// pull in viper.
type Config struct {
	Type     string // file | redis | database | postgres (default: file)
	File     FileOptions
	Redis    RedisOptions
	Database DatabaseOptions
	Postgres PostgresOptions
}

// New constructs the configured backend. The postgres backend stores
// incidents, analyses, and blobs in a single database; the file and redis
// backends keep blobs alongside their other records.
func New(c Config) (Provider, error) {
	t := c.Type
	if t == "" {
		t = "file"
	}

	// POSTGRES_DSN env var fallback for the postgres type.
	if t == "postgres" && c.Postgres.DSN == "" {
		c.Postgres.DSN = os.Getenv("POSTGRES_DSN")
	}

	switch t {
	case "file":
		return NewFile(c.File)
	case "redis":
		return NewRedis(c.Redis)
	case "database":
		return NewDatabase(c.Database)
	case "postgres":
		return NewPostgres(c.Postgres)
	default:
		return nil, fmt.Errorf("storage: unknown type %q (expected file|redis|database|postgres)", t)
	}
}
