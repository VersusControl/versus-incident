package agent

import "testing"

func TestMiner_ClustersSimilarMessages(t *testing.T) {
	m := NewMiner(0.4, 4, 100)

	id1, tmpl1, isNew1 := m.Cluster("connection refused to database server db-01 port 5432")
	if !isNew1 {
		t.Fatalf("first message should be new")
	}
	if id1 == "" || tmpl1 == "" {
		t.Fatalf("expected id+template, got %q %q", id1, tmpl1)
	}

	id2, _, isNew2 := m.Cluster("connection refused to database server db-02 port 5432")
	if isNew2 {
		t.Errorf("second similar message should NOT be a new cluster")
	}
	if id2 != id1 {
		t.Errorf("similar messages should share cluster id, got %s vs %s", id1, id2)
	}

	id3, _, _ := m.Cluster("connection refused to database server db-03 port 5432")
	if id3 != id1 {
		t.Errorf("third similar message should share cluster id, got %s vs %s", id1, id3)
	}

	// Wildly different message → new cluster
	id4, _, isNew4 := m.Cluster("user alice signed in successfully from web client")
	if !isNew4 || id4 == id1 {
		t.Errorf("dissimilar message should produce a new cluster")
	}
}

func TestMiner_TokenizeIgnoresRedactedAsVariable(t *testing.T) {
	m := NewMiner(0.4, 4, 100)
	id1, _, _ := m.Cluster("user <REDACTED:email> failed login from <REDACTED:ipv4>")
	id2, _, isNew := m.Cluster("user <REDACTED:email> failed login from <REDACTED:ipv4>")
	if isNew {
		t.Errorf("identical redacted messages must share a cluster")
	}
	if id1 != id2 {
		t.Errorf("expected same id, got %s vs %s", id1, id2)
	}
}

// TestMiner_Reset proves Reset forgets every learned cluster and the bucket
// tree, so a line the miner had already learned is re-discovered as new (isNew
// again) after the reset — mining truly restarts from scratch.
func TestMiner_Reset(t *testing.T) {
	m := NewMiner(0.4, 4, 100)
	msg := "connection refused to database server db-01 port 5432"

	if _, _, isNew := m.Cluster(msg); !isNew {
		t.Fatalf("first sighting should be new")
	}
	if _, _, isNew := m.Cluster(msg); isNew {
		t.Fatalf("second sighting should not be new before reset")
	}
	if n := len(m.Snapshot()); n != 1 {
		t.Fatalf("miner should hold 1 cluster before reset, got %d", n)
	}

	m.Reset()

	if n := len(m.Snapshot()); n != 0 {
		t.Fatalf("miner should hold 0 clusters after reset, got %d", n)
	}
	if _, _, isNew := m.Cluster(msg); !isNew {
		t.Fatalf("after reset the same line must be re-discovered as new")
	}
}
