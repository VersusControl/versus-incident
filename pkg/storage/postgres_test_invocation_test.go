package storage_test

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCIRunsSharedSchemaPostgresTestsSerially(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/dev.yml")
	if err != nil {
		t.Fatalf("read development workflow: %v", err)
	}
	var config struct {
		Jobs map[string]struct {
			Env      map[string]string `yaml:"env"`
			Services map[string]struct {
				Image   string            `yaml:"image"`
				Env     map[string]string `yaml:"env"`
				Ports   []string          `yaml:"ports"`
				Options string            `yaml:"options"`
			} `yaml:"services"`
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflow, &config); err != nil {
		t.Fatalf("parse development workflow: %v", err)
	}
	testJob, ok := config.Jobs["test"]
	if !ok {
		t.Fatal("development workflow is missing the test job")
	}
	const wantDSN = "postgres://versus_test:versus_test@localhost:5432/versus_test?sslmode=disable"
	if testJob.Env["TEST_POSTGRES_DSN"] != wantDSN {
		t.Fatalf("test TEST_POSTGRES_DSN = %q, want non-secret CI service DSN", testJob.Env["TEST_POSTGRES_DSN"])
	}
	postgres, ok := testJob.Services["postgres"]
	if !ok || !strings.HasPrefix(postgres.Image, "postgres:") {
		t.Fatalf("test Postgres service = %+v, want official postgres image", postgres)
	}
	if postgres.Env["POSTGRES_USER"] != "versus_test" || postgres.Env["POSTGRES_PASSWORD"] != "versus_test" ||
		postgres.Env["POSTGRES_DB"] != "versus_test" {
		t.Fatalf("test Postgres service credentials/database = %+v", postgres.Env)
	}
	if len(postgres.Ports) != 1 || postgres.Ports[0] != "5432:5432" {
		t.Fatalf("test Postgres service ports = %v, want 5432:5432", postgres.Ports)
	}
	for _, option := range []string{"--health-cmd", "pg_isready", "--health-interval", "--health-timeout", "--health-retries"} {
		if !strings.Contains(postgres.Options, option) {
			t.Fatalf("test Postgres service options missing %q: %q", option, postgres.Options)
		}
	}
	var testCommand string
	for _, step := range testJob.Steps {
		if step.Name == "Test" {
			testCommand = strings.Join(strings.Fields(step.Run), " ")
			break
		}
	}
	fields := strings.Fields(testCommand)
	if len(fields) < 6 || fields[0] != "go" || fields[1] != "test" || fields[len(fields)-1] != "./..." ||
		!containsAdjacent(fields, "-p", "1") || !containsField(fields, "-race") {
		t.Fatalf("development workflow test command %q must run all packages with go test -p 1 -race", testCommand)
	}
}

func containsAdjacent(fields []string, first, second string) bool {
	for index := 0; index+1 < len(fields); index++ {
		if fields[index] == first && fields[index+1] == second {
			return true
		}
	}
	return false
}

func containsField(fields []string, want string) bool {
	for _, field := range fields {
		if field == want {
			return true
		}
	}
	return false
}
