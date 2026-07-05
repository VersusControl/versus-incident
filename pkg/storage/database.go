package storage

// DatabaseOptions mirrors the database sub-block of `storage:`. The
// database backend is config-only today — instantiating it returns
// ErrUnsupported. The struct exists so the config layer can validate
// its shape.
type DatabaseOptions struct {
	Driver string // postgres | mysql | sqlite
	DSN    string
}

// NewDatabase is a placeholder that returns ErrUnsupported.
func NewDatabase(_ DatabaseOptions) (Provider, error) {
	return nil, ErrUnsupported
}
