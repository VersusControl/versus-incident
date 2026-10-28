package chat

import (
	"os/exec"
	"strings"
	"testing"
)

func TestEinoDoesNotEscapeIntoControllersOrServices(t *testing.T) {
	for _, pkg := range []string{"../../../controllers", "../../../services"} {
		output, err := exec.Command("go", "list", "-f", `{{join .Imports "\n"}}`, pkg).Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		for _, dependency := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(strings.TrimSpace(dependency), "github.com/cloudwego/eino") {
				t.Fatalf("%s imports Eino dependency %q", pkg, dependency)
			}
		}
	}
}
