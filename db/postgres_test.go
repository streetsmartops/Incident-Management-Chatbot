package db_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/db"
	"github.com/streetsmartops/incident-management-chatbot/test/storetest"
)

var n atomic.Int64

// Set TEST_DATABASE_URL to run against a real Postgres (CI uses a service container).
func dsn(t *testing.T) string {
	d := os.Getenv("TEST_DATABASE_URL")
	if d == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres integration tests")
	}
	return d
}

func TestPostgresConformance(t *testing.T) {
	d := dsn(t)
	storetest.Run(t, func(t *testing.T) core.Store {
		schema := fmt.Sprintf("it_%d_%d", time.Now().UnixNano()%1e9, n.Add(1))
		s, err := db.Open(context.Background(), d, schema)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			s.Close()
			c, err := pgx.Connect(context.Background(), d)
			if err == nil {
				_, _ = c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
				_ = c.Close(context.Background())
			}
		})
		return s
	})
}

func TestPostgresMultiReplicaAppends(t *testing.T) {
	d := dsn(t)
	schema := fmt.Sprintf("it_mr_%d", time.Now().UnixNano()%1e9)
	s, err := db.Open(context.Background(), d, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		s.Close()
		if c, err := pgx.Connect(context.Background(), d); err == nil {
			_, _ = c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
			_ = c.Close(context.Background())
		}
	}()
	storetest.RunMultiReplica(t, s)
}

func TestTimelineIsAppendOnlyInDB(t *testing.T) {
	d := dsn(t)
	ctx := context.Background()
	schema := fmt.Sprintf("it_ao_%d", time.Now().UnixNano()%1e9)
	s, err := db.Open(ctx, d, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	inc := &core.Incident{ID: "i1", Key: "INC-1", State: core.StateDeclared, Roles: map[core.Role]core.UserRef{}, DetectedAt: time.Now()}
	_ = s.CreateIncident(ctx, inc)
	ev, _ := core.NewEvent("i1", nil, time.Now(), core.SystemActor, core.EvNote, map[string]string{"a": "b"})
	if err := s.AppendEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	c, err := pgx.Connect(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	for _, q := range []string{
		"UPDATE " + schema + ".timeline_events SET payload='{}'",
		"DELETE FROM " + schema + ".timeline_events",
		"TRUNCATE " + schema + ".timeline_events",
	} {
		if _, err := c.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s must be blocked, got %v", q, err)
		}
	}
	// Re-open is idempotent (migrations recorded).
	s2, err := db.Open(ctx, d, schema)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if _, err := db.Open(ctx, d, "bad;schema"); err == nil {
		t.Fatal("schema names must be validated")
	}
	// Cleanup needs to bypass the trigger: drop the whole schema.
	_, _ = c.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
}
