package storage

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"
)

func TestZZBackfillTiming(t *testing.T) {
	dsn := os.Getenv("K27_CLONE_DSN")
	if dsn == "" {
		t.Skip()
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	p := &postgresProvider{db: db}
	start := time.Now()
	err = p.completeEvidenceBackfill(context.Background(), evidenceStartupBudget)
	t.Logf("elapsed=%s err=%v", time.Since(start), err)
}
