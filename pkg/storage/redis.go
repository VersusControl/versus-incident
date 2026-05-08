package storage

// RedisOptions mirrors the redis sub-block of `storage:`. The redis
// backend is config-only today — instantiating it returns ErrUnsupported.
// The struct exists so the config layer can validate its shape.
type RedisOptions struct {
	Host               string
	Port               int
	Password           string
	DB                 int
	InsecureSkipVerify bool
	KeyPrefix          string
	MaxIncidents       int
}

// NewRedis is a placeholder that returns ErrUnsupported. The redis
// backend will be implemented alongside its first dependency.
func NewRedis(_ RedisOptions) (Provider, error) {
	return nil, ErrUnsupported
}
