package storage

import "fmt"

// Config mirrors the root-level `storage:` block in config.yaml. It is
// kept here (rather than in pkg/config) so tests in this package don't
// pull in viper.
type Config struct {
	Type     string // file | redis | database (default: file)
	File     FileOptions
	Redis    RedisOptions
	Database DatabaseOptions
}

// New constructs the configured backend.
func New(c Config) (Provider, error) {
	t := c.Type
	if t == "" {
		t = "file"
	}
	switch t {
	case "file":
		return NewFile(c.File)
	case "redis":
		return NewRedis(c.Redis)
	case "database":
		return NewDatabase(c.Database)
	default:
		return nil, fmt.Errorf("storage: unknown type %q (expected file|redis|database)", t)
	}
}
