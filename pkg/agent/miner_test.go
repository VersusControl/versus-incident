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
