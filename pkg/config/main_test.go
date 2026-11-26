package config

import (
	"os"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config/configtest"
)

// TestMain drops config variables inherited from the caller's shell, so the
// tests see only the YAML they write and the variables they set with
// t.Setenv.
func TestMain(m *testing.M) {
	configtest.ClearProcessEnv()
	os.Exit(m.Run())
}
