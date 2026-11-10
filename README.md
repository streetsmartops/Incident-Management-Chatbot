# incidentd: Incident Management Chatbot

Runs the incident lifecycle from chat: declare and coordinate, read-only AI triage,
human-approved remediation, and blameless postmortems. One Go service (`incidentd`); the
offline triage evaluation harness is in Python (`eval/`).

Design: [`docs/architecture.md`](docs/architecture.md). Every vendor detail is listed there
as verified or `UNVERIFIED`.

## Status (v1)

| Area | State |
|---|---|
| Slack (slash command + buttons) | **Live** |
| Microsoft Teams | **Live, optional** (`teams.enabled`); incidents run in the announce conversation (no Graph) |
| PagerDuty (paging, on-call, webhooks v3) | **Live** |
| Datadog (metrics, logs, monitors, events; read-only) | **Live** |
| Jira Cloud (incident tickets, comments, action items) | **Live** |
| Kubernetes (read-only triage, client-side dry-run diffs) | **Live** |
| Multiplexer SLO source (poller, tenant tier, budget) | **Live**; tenant tier needs `MULTIPLEXER_TOKEN` after Multiplexer gap PR G2 |
| Anthropic API (`claude-opus-5-5`) | **Live** |
| Remediation execution | **Built, disabled**: dry-run only until the triage eval gate passes (two-key kill switch) |
| Opsgenie, ServiceNow, Prometheus/Grafana, AWS/GCP/Azure, Bedrock/Vertex, Nobl9/Datadog SLOs | Typed stubs that fail loudly (v2) |

## Commands (identical on Slack and Teams)

Slack: `/incident <verb> …` · Teams: `@IncidentBot <verb> …`

| Verb | Example | Who |
|---|---|---|
| `declare` | `declare "Checkout 500s" --sev 2 --service checkout [--private]` | anyone |
| `sev` | `sev 1 --reason "all regions"` | IC |
| `role` | `role comms @bob` · `role ic me` (vacant IC) | IC |
| `status` | `status` · `status mitigating` | IC to change |
| `update` | `update "Rolled back; watching error rate"` | IC, comms |
| `triage` | `triage --focus "payments"` | participants |
| `run` | `run k8s.rollout_restart cluster=prod-eu-1 namespace=checkout deployment=api` | participants (needs approval) |
| `resolve` | `resolve "Bad deploy rolled back"` | IC |
| `postmortem` | `postmortem draft` · `postmortem publish` | IC |
| `timeline` | `timeline --last 20` | anyone |

Outside a war room (and always on Teams) add `--incident INC-2026-0042`.

## Quick start (local)

Requirements: Go 1.26+, optionally Postgres 16, a Slack workspace where you can install apps, and a tunnel such as ngrok.

```bash
cp config/example.yaml config/local.yaml
```

Edit `config/local.yaml`: set `database.driver: memory` for a throwaway run, and set `slack.announceChannelId`.

```bash
export SLACK_BOT_TOKEN=xoxb-... SLACK_SIGNING_SECRET=...
```

```bash
go run ./cmd/incidentd config-check -config config/local.yaml
```

```bash
go run ./cmd/incidentd serve -config config/local.yaml
```

Expose `:8080` publicly. In `deploy/slack/manifest.yaml`, replace `https://incidentd.example.com` with the
tunnel URL, create the app from the manifest, install it, and invite the bot to the announce channel.

## Configuration and secrets

Non-secret settings: YAML (`config/example.yaml`, fully commented). Unknown keys are rejected,
and secrets are **never** accepted in YAML. Each secret is read from env `NAME` or from the file at
`NAME_FILE` (Kubernetes Secret / Vault agent mount). Every integration is off until enabled,
so keys can be supplied at deployment time. `config-check` reports every problem at once.

| Secret | Needed when |
|---|---|
| `DATABASE_URL` | `database.driver: postgres` |
| `SLACK_BOT_TOKEN`, `SLACK_SIGNING_SECRET` | `slack.enabled` |
| `TEAMS_APP_PASSWORD` | `teams.enabled` |
| `PAGERDUTY_API_TOKEN`, `PAGERDUTY_WEBHOOK_SECRET` | `pagerduty.enabled` |
| `DATADOG_API_KEY`, `DATADOG_APP_KEY` | `datadog.enabled` |
| `JIRA_API_TOKEN` | `jira.enabled` |
| `MULTIPLEXER_TOKEN` | optional until Multiplexer PR G2; without it tenant tier is `unknown` (no auto-declare) |
| `ANTHROPIC_API_KEY` | `llm.enabled` with `provider: anthropic` |
| `INCIDENTD_ADMIN_TOKEN` | admin endpoints |

| Env (non-secret) | Meaning |
|---|---|
| `INCIDENTD_CONFIG` | default config path (`config/config.yaml`) |
| `REMEDIATION_ENABLED` | env half of the kill switch; default off |

Least-privilege credentials:
- **Slack bot scopes:** see the manifest.
- **PagerDuty:** a REST token able to read services and on-calls and create, resolve and annotate incidents. `pagerduty.fromEmail` must be a PagerDuty user.
- **Datadog:** an application key scoped to read metrics, logs, monitors and events.
- **Jira:** an account limited to the configured project.
- **Kubernetes:** `deploy/k8s/rbac-readonly.yaml`.

Severity policy: the built-in matrix is in [`docs/architecture.md` §5.2](docs/architecture.md). For a company
policy, copy `config/severity-policy.example.yaml` and set `severityPolicyFile`. The policy is validated at startup,
and its hash is recorded on every declaration.

## Integrations

**Slack:**
1. Create the app from `deploy/slack/manifest.yaml`. The command uses `should_escape: true` so mentions resolve.
2. Request URLs: `POST /slack/commands`, `POST /slack/interactivity`.

**Teams (optional):** see [`deploy/teams/README.md`](deploy/teams/README.md). Messaging endpoint: `POST /teams/messages`.

**PagerDuty:**
1. Add a Webhooks v3 subscription to `POST /webhooks/pagerduty` and put its signing secret in `PAGERDUTY_WEBHOOK_SECRET`.
2. Map services in `pagerduty.services`.

Pages opened by Multiplexer are titled `SLO Breach: <name>`. They correlate to the guardrail, so one breach yields one incident.

**Multiplexer:**
1. Set `multiplexer.baseUrl`.
2. The poller reads `/api/slo-guardrails` every `pollInterval`.
3. Tenant tier and service owner need `/api/tenants`, which requires a service token (PR G2).

**Kubernetes:**
1. List clusters and a namespace allow-list in `kubernetes.clusters`.
2. Apply `deploy/k8s/rbac-readonly.yaml` with one RoleBinding per allowed namespace.

## Remediation: enabling execution

Execution needs **both** keys: env `REMEDIATION_ENABLED=true` **and** the runtime flag:

```bash
curl -X POST -H "Authorization: Bearer $INCIDENTD_ADMIN_TOKEN" "https://<host>/admin/remediation?enabled=true"
```

Do this only after:
1. The triage eval gate passes on at least 20 incidents, of which at least 10 are historical (see "Evaluation").
2. Security review signs off.
3. `deploy/k8s/rbac-remediation.yaml` is applied for the allowed namespaces.

Until then, approved actions are recorded and shown as dry-run diffs only. Setting the flag to `false` is the global kill switch.

Approval rules:
- **Low risk:** one approver. The requester can self-approve only if they are the IC.
- **Medium risk:** the IC, who must not be the requester.
- **High risk:** the IC **and** the service owner, two different people, neither of them the requester.

Approvals expire after `remediation.approvalTTL`. The service owner comes from `remediation.serviceOwners` or Multiplexer `services.owner`.
Before executing, the engine rebuilds the approvals from the hash-verified timeline.

## Evaluation (triage)

```bash
python3 eval/gen_synthetic.py
```

That command regenerates the 21 synthetic fixtures in `test/fixtures/incidents`. The next one is a billed run against the Anthropic API:

```bash
ANTHROPIC_API_KEY=... go run ./cmd/incidentd eval run -out eval/out/run.jsonl
```

```bash
python3 eval/score.py eval/out/run.jsonl --report eval/out/report.md
```

Historical incidents (90-day cap) are imported as skeletons for a human to label:

```bash
PAGERDUTY_API_TOKEN=... go run ./cmd/incidentd eval import-pagerduty -since 2160h -out test/fixtures/historical
```

Reports with fewer than 10 historical incidents are labelled **NOT REPRESENTATIVE** and cannot pass the gate.
Keep `test/fixtures/historical` out of git if it contains customer data.

## Deploy

```bash
docker build -t incidentd .
```

```bash
kubectl apply -f deploy/k8s/namespace.yaml -f deploy/k8s/rbac-readonly.yaml -f deploy/k8s/deployment.yaml
```

- Create `incidentd-secrets` with your secret manager; `deploy/k8s/secret.example.yaml` shows the keys.
- Fill the ConfigMap from `config/example.yaml`.
- Probes: `GET /healthz` (liveness) and `GET /readyz` (database).
- Multiple replicas are safe for the timeline: appends are serialized per incident and conflicts across replicas are retried.
- The Multiplexer poller runs in each replica. Dedupe prevents duplicate incidents, but polls are duplicated; one replica is enough for v1 load.

Database: a dedicated schema (`database.schema`, default `incident_bot`), migrated at startup under an advisory lock.
The suggested grants are in [`db/migrations/README.md`](db/migrations/README.md). `timeline_events` is append-only, enforced by triggers.

## Operations

| Task | How |
|---|---|
| Verify an incident's audit chain | `incidentd verify-timeline -config … INC-2026-0042` or `GET /admin/timeline/verify?incident=…` |
| Kill remediation | `POST /admin/remediation?enabled=false`, or set `REMEDIATION_ENABLED=false` |
| Validate config | `incidentd config-check -config …` |
| Logs | JSON on stdout with `incident_id`, `correlation_id`, `request_id` |

## Development

```bash
make test
```

```bash
TEST_DATABASE_URL=postgres://postgres@localhost:5432/postgres?sslmode=disable make test-db
```

- `make test` runs the Go tests plus the Python eval tests.
- `make test-db` runs the Postgres integration tests.
- CI (`.github/workflows/ci.yml`) runs gofmt, `go vet`, `go test -race` against a Postgres 16 service, pytest, and a fixture drift check.
- Changes to the catalog, RBAC and severity policy need SRE lead review (`.github/CODEOWNERS`).

Layout: `core/` (domain, lifecycle, ports, service) · `chat/` · `providers/` · `ingest/` · `triage/` ·
`remediation/` (+ `catalog/`) · `postmortem/` · `config/` · `db/` · `cmd/incidentd/` · `eval/` · `test/` · `deploy/`.
