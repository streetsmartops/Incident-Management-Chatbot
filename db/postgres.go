// Package db implements core.Store on PostgreSQL (dedicated schema) with
// embedded migrations.
package db

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/streetsmartops/incident-management-chatbot/core"
)

//go:embed migrations/*.sql
var migrations embed.FS

var identRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Store is the Postgres core.Store.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects with search_path set to schema, creates the schema if
// allowed, and applies migrations.
func Open(ctx context.Context, dsn, schema string) (*Store, error) {
	if !identRe.MatchString(schema) {
		return nil, fmt.Errorf("invalid schema name %q", schema)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.RuntimeParams["application_name"] = "incidentd"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx, schema); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks connectivity (readiness probe).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) migrate(ctx context.Context, schema string) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('incidentd_migrate'))`); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('incidentd_migrate'))`) //nolint:errcheck
	if _, err := conn.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+schema); err != nil {
		return fmt.Errorf("create schema %s (pre-create it if the role lacks CREATE): %w", schema, err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sqlBytes, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return core.ErrNotFound
	}
	return err
}

func (s *Store) NextIncidentNumber(ctx context.Context, year int) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `INSERT INTO incident_counters(year, n) VALUES ($1, 1)
		ON CONFLICT (year) DO UPDATE SET n = incident_counters.n + 1 RETURNING n`, year).Scan(&n)
	return n, err
}

func incidentArgs(inc *core.Incident) ([]byte, *string, *string, []string) {
	data, _ := json.Marshal(inc)
	var p, id *string
	if inc.WarRoom != nil {
		p, id = &inc.WarRoom.Platform, &inc.WarRoom.ID
	}
	keys := inc.DedupKeys
	if keys == nil {
		keys = []string{}
	}
	return data, p, id, keys
}

func (s *Store) CreateIncident(ctx context.Context, inc *core.Incident) error {
	data, p, id, keys := incidentArgs(inc)
	_, err := s.pool.Exec(ctx, `INSERT INTO incidents(id, key, state, severity, dedup_keys, war_room_platform, war_room_id, detected_at, data)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, inc.ID, inc.Key, string(inc.State), int(inc.Severity), keys, p, id, inc.DetectedAt, data)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return core.ErrConflict
	}
	return err
}

func (s *Store) UpdateIncident(ctx context.Context, inc *core.Incident) error {
	data, p, id, keys := incidentArgs(inc)
	tag, err := s.pool.Exec(ctx, `UPDATE incidents SET state=$2, severity=$3, dedup_keys=$4, war_room_platform=$5, war_room_id=$6, data=$7, updated_at=now() WHERE id=$1`,
		inc.ID, string(inc.State), int(inc.Severity), keys, p, id, data)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return core.ErrNotFound
	}
	return nil
}

func scanIncident(row pgx.Row) (*core.Incident, error) {
	var data []byte
	if err := row.Scan(&data); err != nil {
		return nil, notFound(err)
	}
	var inc core.Incident
	if err := json.Unmarshal(data, &inc); err != nil {
		return nil, err
	}
	if inc.Roles == nil {
		inc.Roles = map[core.Role]core.UserRef{}
	}
	return &inc, nil
}

func (s *Store) GetIncident(ctx context.Context, id string) (*core.Incident, error) {
	return scanIncident(s.pool.QueryRow(ctx, `SELECT data FROM incidents WHERE id=$1`, id))
}

func (s *Store) FindIncidentByKey(ctx context.Context, key string) (*core.Incident, error) {
	return scanIncident(s.pool.QueryRow(ctx, `SELECT data FROM incidents WHERE key=$1`, key))
}

func (s *Store) FindIncidentByChannel(ctx context.Context, ch core.ChannelRef) (*core.Incident, error) {
	return scanIncident(s.pool.QueryRow(ctx, `SELECT data FROM incidents WHERE war_room_platform=$1 AND war_room_id=$2 ORDER BY detected_at DESC LIMIT 1`, ch.Platform, ch.ID))
}

var closedStates = []string{string(core.StateResolved), string(core.StatePostmortem), string(core.StateClosed), string(core.StateDismissed)}

func (s *Store) FindOpenIncidentByDedupKey(ctx context.Context, key string) (*core.Incident, error) {
	return scanIncident(s.pool.QueryRow(ctx, `SELECT data FROM incidents WHERE $1 = ANY(dedup_keys) AND NOT (state = ANY($2)) ORDER BY detected_at DESC LIMIT 1`, key, closedStates))
}

func (s *Store) ListIncidents(ctx context.Context, f core.IncidentFilter) ([]*core.Incident, error) {
	q := `SELECT data FROM incidents`
	args := []any{}
	if len(f.States) > 0 {
		st := make([]string, len(f.States))
		for i, x := range f.States {
			st[i] = string(x)
		}
		q += ` WHERE state = ANY($1)`
		args = append(args, st)
	}
	q += ` ORDER BY detected_at DESC`
	if f.Limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, f.Limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*core.Incident
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

func (s *Store) AppendEvent(ctx context.Context, ev core.TimelineEvent) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO timeline_events(incident_id, seq, at, actor_type, actor_id, actor_email, kind, payload, prev_hash, hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		ev.IncidentID, ev.Seq, ev.At, string(ev.Actor.Type), ev.Actor.ID, ev.Actor.Email, string(ev.Kind), string(ev.Payload), ev.PrevHash, ev.Hash)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return core.ErrConflict
	}
	return err
}

func scanEvent(row pgx.Row) (core.TimelineEvent, error) {
	var ev core.TimelineEvent
	var at time.Time
	var actorType, kind, payload string
	err := row.Scan(&ev.IncidentID, &ev.Seq, &at, &actorType, &ev.Actor.ID, &ev.Actor.Email, &kind, &payload, &ev.PrevHash, &ev.Hash)
	ev.At, ev.Actor.Type, ev.Kind, ev.Payload = at.UTC(), core.ActorType(actorType), core.EventKind(kind), json.RawMessage(payload)
	return ev, err
}

const eventCols = `incident_id, seq, at, actor_type, actor_id, actor_email, kind, payload, prev_hash, hash`

func (s *Store) LastEvent(ctx context.Context, id string) (*core.TimelineEvent, error) {
	ev, err := scanEvent(s.pool.QueryRow(ctx, `SELECT `+eventCols+` FROM timeline_events WHERE incident_id=$1 ORDER BY seq DESC LIMIT 1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ev, nil
}

func (s *Store) Events(ctx context.Context, id string) ([]core.TimelineEvent, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+eventCols+` FROM timeline_events WHERE incident_id=$1 ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.TimelineEvent
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Store) SaveSignal(ctx context.Context, sig core.Signal) error {
	data, _ := json.Marshal(sig)
	_, err := s.pool.Exec(ctx, `INSERT INTO signals(id, dedup_key, observed_at, data) VALUES ($1,$2,$3,$4)
		ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data`, sig.ID, sig.DedupKey, sig.ObservedAt, data)
	return err
}

func (s *Store) GetSignal(ctx context.Context, id string) (*core.Signal, error) {
	var data []byte
	if err := s.pool.QueryRow(ctx, `SELECT data FROM signals WHERE id=$1`, id).Scan(&data); err != nil {
		return nil, notFound(err)
	}
	var sig core.Signal
	return &sig, json.Unmarshal(data, &sig)
}

func (s *Store) RecordObservation(ctx context.Context, key string, status core.SloStatus) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `INSERT INTO observations(dedup_key, status, count) VALUES ($1,$2,1)
		ON CONFLICT (dedup_key) DO UPDATE SET
		  count = CASE WHEN observations.status = EXCLUDED.status THEN observations.count + 1 ELSE 1 END,
		  status = EXCLUDED.status
		RETURNING count`, key, string(status)).Scan(&n)
	return n, err
}

func (s *Store) SaveAction(ctx context.Context, a *core.ActionRequest) error {
	data, _ := json.Marshal(a)
	_, err := s.pool.Exec(ctx, `INSERT INTO action_requests(id, incident_id, data) VALUES ($1,$2,$3)
		ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data, updated_at = now()`, a.ID, a.IncidentID, data)
	return err
}

func (s *Store) GetAction(ctx context.Context, id string) (*core.ActionRequest, error) {
	var data []byte
	if err := s.pool.QueryRow(ctx, `SELECT data FROM action_requests WHERE id=$1`, id).Scan(&data); err != nil {
		return nil, notFound(err)
	}
	var a core.ActionRequest
	return &a, json.Unmarshal(data, &a)
}

func (s *Store) SavePostmortem(ctx context.Context, p *core.Postmortem) error {
	data, _ := json.Marshal(p)
	_, err := s.pool.Exec(ctx, `INSERT INTO postmortems(incident_id, data) VALUES ($1,$2)
		ON CONFLICT (incident_id) DO UPDATE SET data = EXCLUDED.data, updated_at = now()`, p.IncidentID, data)
	return err
}

func (s *Store) GetPostmortem(ctx context.Context, id string) (*core.Postmortem, error) {
	var data []byte
	if err := s.pool.QueryRow(ctx, `SELECT data FROM postmortems WHERE incident_id=$1`, id).Scan(&data); err != nil {
		return nil, notFound(err)
	}
	var p core.Postmortem
	return &p, json.Unmarshal(data, &p)
}

func (s *Store) GetFlag(ctx context.Context, name string) (bool, error) {
	var v bool
	err := s.pool.QueryRow(ctx, `SELECT value FROM flags WHERE name=$1`, name).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return v, err
}

func (s *Store) SetFlag(ctx context.Context, name string, v bool) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO flags(name, value) VALUES ($1,$2) ON CONFLICT (name) DO UPDATE SET value=EXCLUDED.value, updated_at=now()`, name, v)
	return err
}

var _ core.Store = (*Store)(nil)
