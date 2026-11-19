package snapshot

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// These tests pin the change feed SQL that differs between SQLite and
// Postgres: minute counts merge by maximum on a re-read (MAX on SQLite,
// GREATEST on Postgres), and minute buckets are UTC minutes whatever the
// database session's time zone (to_char … AT TIME ZONE 'UTC').

// A re-read of the same minute never lowers or doubles its count: the
// stored count is the largest any read saw.
func TestRereadMinuteCountsMergeByMaximum(t *testing.T) {
	eachBackend(t, func(t *testing.T) {
		fixture := newFeedFixture(t, scopeOf(region))
		minute := fixture.clock.Now().Add(-2 * time.Hour).Truncate(time.Minute)
		covered := []changes.Interval{{From: minute, To: minute.Add(time.Minute)}}
		stored := func() int64 {
			t.Helper()
			feed, err := fixture.provider.AWSChangeFeed(context.Background(), tenancy.DefaultOrgScope(),
				storage.AWSChangeFeedQuery{Since: minute, Until: minute.Add(time.Minute)})
			if err != nil {
				t.Fatal(err)
			}
			var total int64
			for _, count := range feed.Counts {
				total += count.Events
			}
			return total
		}
		for _, step := range []struct{ read, want int64 }{{5, 5}, {3, 5}, {5, 5}, {7, 7}} {
			fixture.poll(account, region, ChangePoll{Covered: covered, Counts: []storage.AWSChangeCount{{Kind: storage.AWSChangeCountRoutine,
				EventName: "UpdateInstanceInformation", EventSource: "ssm.amazonaws.com", Service: "ssm", ActorClass: string(model.ActorAWSService),
				Minute: minute, Events: step.read}}})
			if got := stored(); got != step.want {
				t.Fatalf("after a read of %d the minute holds %d, want %d", step.read, got, step.want)
			}
		}
	})
}

// A Postgres session in a time zone far from UTC (Chatham, +13:45) still
// buckets each change into its UTC minute.
func TestPostgresMinuteBucketsIgnoreSessionTimeZone(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("timezone", "Pacific/Chatham")
	parsed.RawQuery = query.Encode()
	provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: parsed.String()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	db := provider.(storage.SQLAccessor).DB()
	var zone string
	if err := db.QueryRow(`SELECT current_setting('TimeZone')`).Scan(&zone); err != nil || zone != "Pacific/Chatham" {
		t.Fatalf("session time zone = %q, %v; the check needs a non-UTC session", zone, err)
	}
	if _, err := db.Exec(`TRUNCATE aws_account_epochs, aws_sync_units, aws_unit_generations, aws_resources,
 aws_health, aws_security_flags, aws_changes, aws_change_coverage, aws_change_counts, aws_change_gaps, aws_change_pairs`); err != nil {
		t.Fatal(err)
	}
	previous := openBackend
	openBackend = func(*testing.T) inventoryProvider { return provider.(inventoryProvider) }
	t.Cleanup(func() { openBackend = previous })

	fixture := newFeedFixture(t, scopeOf(region))
	at := fixture.clock.Now().Add(-3 * time.Hour).Truncate(time.Minute).Add(30 * time.Second)
	fixture.pollSettled(account, region, feedChange("tz-1", at, model.ActorPerson, "arn:aws:iam::111122223333:user/ops"))
	feed, err := fixture.provider.AWSChangeFeed(context.Background(), tenancy.DefaultOrgScope(),
		storage.AWSChangeFeedQuery{Since: at.Add(-time.Hour), Until: fixture.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Minutes) != 1 || !feed.Minutes[0].Minute.Equal(at.Truncate(time.Minute)) || feed.Minutes[0].Events != 1 {
		t.Fatalf("minutes = %+v, want one change in %s UTC", feed.Minutes, at.Truncate(time.Minute).Format(time.RFC3339))
	}
	answer := fixture.answer(FeedRequest{})
	var bucketed int64
	for _, bucket := range answer.Buckets {
		if !at.Before(bucket.Start) && at.Before(bucket.Start.Add(time.Hour)) {
			bucketed = bucket.Person
		}
	}
	if bucketed != 1 || answer.Summary.Total != 1 {
		t.Fatalf("hour bucket holding %s has %d person changes (total %d), want 1", at.Format(time.RFC3339), bucketed, answer.Summary.Total)
	}
}
