package tools

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
)

func hasTool(tools []core.AnalyzeTool, name string) bool {
	for _, t := range tools {
		if t.Name() == name {
			return true
		}
	}
	return false
}

// TestDefault_FindRunbookRegistration verifies find_runbook is wired
// only when BOTH the embedder and the runbook searcher are present, so a
// community install with no embeddings configured never registers it.
func TestDefault_FindRunbookRegistration(t *testing.T) {
	emb := &recordingEmbedder{}
	idx := &fakeSearcher{}

	cases := []struct {
		name     string
		embedder core.Embedder
		runbooks RunbookSearcher
		want     bool
	}{
		{"both present", emb, idx, true},
		{"embedder only", emb, nil, false},
		{"searcher only", nil, idx, false},
		{"neither", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools := Default(nil, nil, nil, nil, nil, nil, nil, tc.embedder, tc.runbooks)
			if got := hasTool(tools, "find_runbook"); got != tc.want {
				t.Errorf("find_runbook registered = %v, want %v", got, tc.want)
			}
		})
	}
}
