# Migrations

Applied in filename order by `incidentd` at startup (`db.Migrate`) under a
Postgres advisory lock, recorded in `schema_migrations`. Never edit an applied
migration; add a new file.

Recommended grants for the application role (run as the schema owner/DBA):

```sql
CREATE SCHEMA IF NOT EXISTS incident_bot AUTHORIZATION incidentd_owner;
GRANT USAGE ON SCHEMA incident_bot TO incidentd_app;
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA incident_bot TO incidentd_app;
REVOKE UPDATE, DELETE, TRUNCATE ON incident_bot.timeline_events FROM incidentd_app;
```

The `timeline_events` triggers also block UPDATE/DELETE/TRUNCATE for the owner.
