// Package configtest keeps tests that load the Versus config independent of
// the shell that runs them.
//
// config.LoadConfig applies environment overrides on top of the YAML file
// (explicit variables such as AGENT_AI_ENABLE, ${VAR} expansion in the
// embedded defaults, and viper's automatic key-to-variable binding). A
// developer shell or harness that exports those variables would otherwise
// change what a committed test sees. Tests call Isolate, or ClearProcessEnv
// from TestMain, so the config they load comes only from the YAML they wrote
// and the variables they set themselves with t.Setenv.
package configtest

import (
	"os"
	"strings"
	"testing"
)

// envPrefixes lists the variable name prefixes the config loader reads.
// Each top-level config section is bound by viper's AutomaticEnv under its
// upper-cased name, and the embedded defaults expand ${VAR} placeholders for
// the alert channels and on-call providers. Variables owned by other tools
// (AWS credentials, HARNESS_*, VERSUS_DEVOPS_BIN, KUBERNETES_SERVICE_*) are
// deliberately not listed.
var envPrefixes = []string{
	"AGENT_",
	"ALERT_",
	"AWS_INCIDENT_MANAGER_",
	"EMAIL_",
	"GATEWAY_",
	"INCIDENTIO_",
	"KUBERNETES_EVIDENCE_",
	"LARK_",
	"MSTEAMS_",
	"ONCALL_",
	"PAGERDUTY_",
	"POSTGRES_",
	"PROXY_",
	"QUEUE_",
	"REDIS_",
	"SERVICENOW_",
	"SLACK_",
	"SMTP_",
	"SNS_",
	"STORAGE_",
	"TELEGRAM_",
	"VERSUS_CONNECTORS_",
	"VIBER_",
}

// envNames lists the exact variable names the config loader reads that no
// prefix above covers.
var envNames = map[string]bool{
	"CONNECTOR_SECRET": true,
	"DEBUG_BODY":       true,
	"HOST":             true,
	"NAME":             true,
	"PORT":             true,
	"PUBLIC_HOST":      true,
}

// IsConfigEnv reports whether the config loader reads the named variable.
func IsConfigEnv(name string) bool {
	if envNames[name] {
		return true
	}
	for _, prefix := range envPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// configEnvNames returns the names of every config variable currently set
// in the process environment.
func configEnvNames() []string {
	var names []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if IsConfigEnv(name) {
			names = append(names, name)
		}
	}
	return names
}

// Isolate unsets every config variable for the duration of t and restores
// the caller's values at cleanup. A variable is removed rather than set to
// empty because the loader treats an empty value as "set" for some keys
// (for example AGENT_AI_BASE_URL). Like t.Setenv, it must not be used in a
// parallel test.
func Isolate(t testing.TB) {
	t.Helper()
	for _, name := range configEnvNames() {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
}

// ClearProcessEnv unsets every config variable for the rest of the process.
// Call it from TestMain before m.Run so no test in the package, including
// one that happens to trigger the one-shot global config load, inherits the
// caller's shell configuration.
func ClearProcessEnv() {
	for _, name := range configEnvNames() {
		_ = os.Unsetenv(name)
	}
}
