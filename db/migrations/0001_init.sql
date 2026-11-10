-- incidentd schema v1. Applied inside the configured dedicated schema
-- (search_path is set by the application; default schema: incident_bot).

CREATE TABLE incident_counters (
    year INTEGER PRIMARY KEY,
    n    INTEGER NOT NULL
);

CREATE TABLE incidents (
    id                TEXT PRIMARY KEY,
    key               TEXT NOT NULL UNIQUE,
    state             TEXT NOT NULL,
    severity          SMALLINT NOT NULL,
    dedup_keys        TEXT[] NOT NULL DEFAULT '{}',
    war_room_platform TEXT,
    war_room_id       TEXT,
    detected_at       TIMESTAMPTZ NOT NULL,
    data              JSONB NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX incidents_state_idx      ON incidents (state);
CREATE INDEX incidents_dedup_idx      ON incidents USING GIN (dedup_keys);
CREATE INDEX incidents_war_room_idx   ON incidents (war_room_platform, war_room_id);
CREATE INDEX incidents_detected_idx   ON incidents (detected_at DESC);

-- Append-only, hash-chained audit timeline. payload is TEXT (not JSONB) so
-- the exact bytes that were hashed are preserved.
CREATE TABLE timeline_events (
    incident_id  TEXT NOT NULL REFERENCES incidents(id),
    seq          BIGINT NOT NULL,
    at           TIMESTAMPTZ NOT NULL,
    actor_type   TEXT NOT NULL,
    actor_id     TEXT NOT NULL,
    actor_email  TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL,
    payload      TEXT NOT NULL,
    prev_hash    TEXT NOT NULL,
    hash         TEXT NOT NULL,
    PRIMARY KEY (incident_id, seq)
);

CREATE FUNCTION timeline_events_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'timeline_events is append-only (% blocked)', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER timeline_events_no_update BEFORE UPDATE OR DELETE ON timeline_events
    FOR EACH ROW EXECUTE FUNCTION timeline_events_immutable();
CREATE TRIGGER timeline_events_no_truncate BEFORE TRUNCATE ON timeline_events
    FOR EACH STATEMENT EXECUTE FUNCTION timeline_events_immutable();

CREATE TABLE signals (
    id          TEXT PRIMARY KEY,
    dedup_key   TEXT NOT NULL DEFAULT '',
    observed_at TIMESTAMPTZ NOT NULL,
    data        JSONB NOT NULL
);
CREATE INDEX signals_dedup_idx ON signals (dedup_key, observed_at DESC);

CREATE TABLE observations (
    dedup_key TEXT PRIMARY KEY,
    status    TEXT NOT NULL,
    count     INTEGER NOT NULL
);

CREATE TABLE action_requests (
    id          TEXT PRIMARY KEY,
    incident_id TEXT NOT NULL REFERENCES incidents(id),
    data        JSONB NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE postmortems (
    incident_id TEXT PRIMARY KEY REFERENCES incidents(id),
    data        JSONB NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE flags (
    name       TEXT PRIMARY KEY,
    value      BOOLEAN NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
