# Incident Management Chatbot — Architecture & Design (v1 proposal)

Status: **DRAFT — awaiting approval (Step 1 gate).** No code is written until this is approved.
Owner: Platform/SRE · Last updated: 2026-11-10

Conventions: `UNVERIFIED` = a vendor detail I could not check against primary docs in this
session (doc hosts for Slack, PagerDuty and Microsoft were blocked by the network policy).
Everything about Multiplexer is verified from its source code (commit `e14d5ca`, `main`).

---

## 0. Agreed scope (Step 0 outcome)

| Decision | Choice |
|---|---|
| Chat | **Slack** (primary, v1). **Teams** optional: adapter built behind `chat.teams.enabled` |
| Paging / Observability / ITSM | PagerDuty / Datadog / Jira — live in v1 |
| Runtime | Kubernetes **read-only** live in v1. AWS: typed stub (v2) |
| SLO source | Multiplexer (`streetsmartops/Multi-slo-observability-app`) |
| Identity | Email match: Slack/Teams profile email = PagerDuty user email = Azure AD UPN |
| Approvals | low = 1 approver · medium = IC ≠ requester · high = IC + service owner (2 distinct people) |
| Auto-declare | **SEV1/SEV2 only**; SEV3/4 are proposed with one-click declare |
| LLM | Anthropic API (`claude-opus-5-5`) via `LlmPort`; Bedrock/Vertex adapters as stubs, private-endpoint ready |
| Remediation v1 | Catalog + approval engine built and tested; **executors disabled (dry-run only)** |
| Stack | **Go** (core service) + **Python** (AI engines) — see §2.1 |
| Multiplexer PRs | Against `main`, each flagging that `dev` (per its CLAUDE.md) does not exist |
| Severity policy | Default matrix (§5), overridable by a company policy file |
| Triage eval | Synthetic fixtures now; optional historical import capped at **90 days** |

---

## 1. Reference architecture

```mermaid
flowchart LR
  subgraph Chat
    SL[Slack Events / Slash / Interactivity]
    TM[Teams Bot Framework<br/>optional]
  end
  subgraph Sources
    PDW[PagerDuty Webhooks v3]
    MPX[Multiplexer<br/>/api/slo-guardrails poll]
    DDW[Datadog monitor webhook<br/>v2]
  end

  subgraph incidentd["incidentd (Go)"]
    CA[chat/ adapters<br/>verify + normalise]
    IN[ingest/<br/>verify + normalise + dedupe]
    CORE[core/<br/>state machine · severity policy<br/>append-only timeline · RBAC]
    REM[remediation/<br/>catalog · approvals · executor DISABLED]
    TG[core/tools<br/>read-only tool gateway<br/>redaction]
    P[(ports → providers/<br/>pagerduty · datadog · jira<br/>kubernetes · multiplexer)]
    DB[(Postgres)]
  end

  subgraph brain["brain (Python)"]
    TR[triage/<br/>agent loop · schema validation]
    PM[postmortem/<br/>drafter · BM25 retrieval]
    LLM[LlmPort → Anthropic API<br/>Bedrock/Vertex stubs]
  end

  SL --> CA
  TM --> CA
  PDW --> IN
  MPX --> IN
  DDW -.v2.-> IN
  CA --> CORE
  IN --> CORE
  CORE <--> DB
  CORE --> REM
  CORE -- "triage / draft request" --> TR
  CORE -- "triage / draft request" --> PM
  TR -- "read-only tool calls" --> TG
  PM -- "timeline + metrics read" --> TG
  TG --> P
  REM --> P
  CORE --> P
  TR --> LLM
  PM --> LLM
```

### 2.1 Why Go + Python (and where the line is)

You picked both. To avoid two codebases each talking to every vendor, the split is strict:

| Service | Language | Owns | Holds credentials for |
|---|---|---|---|
| `incidentd` | Go | Chat adapters, ingest, state machine, timeline, severity, approvals, remediation executor, **all vendor adapters**, read-only tool gateway, Postgres | Slack, Teams, PagerDuty, Datadog, Jira, Kubernetes, Multiplexer, DB |
| `brain` | Python | Triage agent loop, postmortem drafting, BM25 retrieval, eval harness, `LlmPort` | **LLM only** + a scoped token for the tool gateway |

Consequences:
- The model can only ever reach vendors through `incidentd`'s **read-only tool gateway**. The
  Python process has no vendor credentials, so a compromised or prompt-injected triage loop cannot
  write anywhere.
- Redaction happens in Go **before** tool output leaves `incidentd`, so raw secrets/PII never reach the
  Python process or the LLM.
- Contracts live in `schemas/*.json` (JSON Schema). Go structs and Pydantic models are generated from
  them; CI fails if either side drifts.
- `brain` is stateless. `incidentd` persists every request/response to the timeline.
- Transport: internal HTTP + JSON, mutual auth by mTLS in-cluster (fallback: HMAC-signed service
  token with 60 s skew window).

Alternative I considered: TypeScript monolith (matches Multiplexer). Rejected because you chose Python/Go.
Alternative 2: everything in Go including the agent loop. Viable — the Anthropic Go SDK exists —
but Python is stronger for the eval harness and BM25/pgvector work. Open question Q1.

### 2.2 Repository layout

```
cmd/incidentd/            Go entrypoint
core/                     Go: domain model, state machine, timeline, severity policy, RBAC, ports (interfaces), tools gateway
chat/slack/  chat/teams/  Go chat adapters (Teams behind feature flag)
providers/                Go adapters: pagerduty/ datadog/ jira/ kubernetes/ multiplexer/
                          stubs (fail loudly): opsgenie/ servicenow/ prometheus/ aws/ gcp/ azure/
ingest/                   Go: webhook handlers, normalisers, Multiplexer poller, dedupe
remediation/              Go: catalog loader, approval engine, executor (disabled), catalog/*.yaml
triage/                   Python: agent loop, tool client, output schema, eval/
postmortem/               Python: drafter, retrieval (BM25 v1, pgvector v2)
config/                   example config, severity-policy.yaml, approval-policy.yaml
db/migrations/            SQL migrations (Postgres 16)
schemas/                  JSON Schema contracts shared by Go and Python
test/fixtures/            recorded webhook payloads, Multiplexer responses, synthetic incidents
deploy/                   Helm chart, Slack app manifest, Teams app manifest
```

Config validation: the master prompt says "Zod-validated". Zod is TypeScript. The equivalent here is
typed config structs + `go-playground/validator` (Go) and Pydantic v2 (Python), both validating the
same `schemas/config.schema.json`. Startup fails with field-level errors if config is invalid.

---

## 3. Ports (core never imports a vendor SDK)

All Go ports live in `core/ports`. Each returns domain types only; vendor types never escape `providers/<x>/`.

| Port | Methods (v1) | v1 adapters | Stubs |
|---|---|---|---|
| `ChatPort` | `VerifyInbound(req) → Inbound` · `PostMessage(ch, Msg) → MsgRef` · `UpdateMessage(ref, Msg)` · `CreateWarRoom(name, private) → ChannelRef` · `Invite(ch, []UserRef)` · `SetTopic(ch, text)` · `PostApprovalCard(ch, ActionRequest) → MsgRef` · `LookupUserByEmail(email) → UserRef` · `UserEmail(UserRef) → email` | Slack | Teams (flagged, see §9 risk R2) |
| `PagingProvider` | `VerifyWebhook(req) → []Signal` · `WhoIsOnCall(serviceRef) → []Responder` · `Page(serviceRef, Incident, urgency) → PageRef` · `Acknowledge(PageRef)` · `Resolve(PageRef)` · `AddNote(PageRef, text)` | PagerDuty | Opsgenie |
| `ObservabilityProvider` | `Capabilities() → set` · `QueryMetric(q, from, to) → Series` · `SearchLogs(q, from, to, limit) → []LogLine` · `MonitorsInState(states, tags) → []Monitor` · `Events(tags, from, to) → []Event` | Datadog | Prometheus/Alertmanager, Grafana |
| `ItsmProvider` | `CreateIncidentTicket(Incident) → TicketRef` · `Update(TicketRef, fields)` · `Comment(TicketRef, text)` · `CreateActionItem(Postmortem, item) → TicketRef` | Jira Cloud | ServiceNow |
| `CloudProvider` | Read: `ListWorkloads(scope)` · `GetWorkload(ref)` · `Events(ref, since)` · `RolloutHistory(ref)` · `PodLogs(ref, since, tail)`. Write: `DryRun(Action) → Diff` · `Execute(Action) → Result` (**returns `ErrExecutorDisabled` in v1**) | Kubernetes (read) | AWS, GCP, Azure |
| `SloSource` | `ListSlos() → []Slo` · `GetSlo(id) → Slo` · `SloEvents(id, limit) → []SloEvent` · `TenantFor(sloID) → (Tenant, ok)` · `BudgetBurned(id, from, to) → (pct, quality)` | Multiplexer | Datadog SLOs, Nobl9 |
| `LlmPort` (Python) | `complete(system, messages, tools, output_schema, budget) → Completion` | Anthropic API | Bedrock, Vertex (private endpoints: PrivateLink / Private Service Connect) |

`Capabilities()` drives which triage tools are exposed (§6.1): a tool is hidden if no configured adapter supports it.

### 3.1 Multiplexer `SloSource` mapping (verified from code)

| Port method | Multiplexer call | Notes |
|---|---|---|
| `ListSlos` | `GET /api/slo-guardrails` → `SloGuardrail[]` | No auth today (gap G1) |
| `GetSlo` | `GET /api/slo-guardrails/:id` → `{guardrail, events}` | |
| `SloEvents` | `GET /api/slo-guardrails/:id/events?limit=N` (N ≤ 500) → `{guardrail_id, events}` | `SloEvent` carries `uptime_percent`, `burn_rate`, `error_budget_remaining_percent` |
| `TenantFor` | v1: `GET /api/tenants` + `GET /api/tenants/:id/views` and join on `view_service_id → view_id` (requires gap G2 for auth) · after PR G3: `tenant` embedded in guardrail | `tier ∈ {enterprise, business, starter}` |
| `BudgetBurned` | difference of `error_budget_remaining_percent` between nearest events around the window | `quality=stale` until PR G4 (events only written on status change) |

Status mapping: Multiplexer `SloStatus` `ok | at_risk | breached | no_data | timeout` → `Signal.sloStatus` (same values).
Correlation: Multiplexer pages PagerDuty with `dedup_key = slo-guardrail-{id}` (`notifications/index.ts:30`). The
PagerDuty webhook normaliser extracts that key to link PD incidents to the guardrail, so the same breach seen
via PD and via the poller produces **one** signal.

---

## 4. Domain model

| Entity | Key fields |
|---|---|
| `Incident` | `id` (ULID), `key` (`INC-2026-0042`), `title`, `severity` SEV1–4, `state`, `services[]`, `tenants[]`, `roles{ic, comms, ops, scribe}`, `warRoom{platform, channelRef}`, `pageRefs[]`, `ticketRef`, `sloRefs[]`, `impactStartedAt`, `detectedAt`, `declaredAt`, `acknowledgedAt`, `mitigatedAt`, `resolvedAt`, `closedAt`, `statusCadence` (e.g. 30 m for SEV1), `correlationId` |
| `Signal` | `id`, `source` (pagerduty·multiplexer·datadog·manual), `externalId`, `dedupKey`, `service`, `tenant?`, `sloStatus?`, `severityHint`, `observedAt`, `payloadRef` (raw stored in blob table, redacted), `incidentId?` |
| `TimelineEvent` | `seq` (per incident, monotonic), `incidentId`, `at`, `actor{type: human·bot·system, id, email}`, `kind` (declared, sev_changed, role_assigned, status_posted, signal_attached, triage_requested, triage_result, action_requested, action_approved, action_denied, action_expired, action_dry_run, action_executed, transition, note, …), `payload` (JSON), `prevHash`, `hash` (SHA-256 chain) |
| `ActionRequest` | `id`, `incidentId`, `actionId` (catalog), `params`, `risk`, `requestedBy`, `source` (human·triage-recommendation), `dryRunDiff`, `approvals[{by, role, at}]`, `requiredApprovals`, `expiresAt`, `status` (pending·approved·denied·expired·executed·failed·verified), `verifyResult` |
| `Postmortem` | `incidentId`, `status` (draft·in_review·published), `sections{…}`, `metrics{ttd, tta, ttDeclare, ttm, ttr, budgetBurnedPct, budgetQuality}`, `actionItems[{title, owner, ticketRef, due}]`, `similarIncidents[{id, score}]` |

Timeline is **append-only**: the app's DB role has `INSERT, SELECT` only on `timeline_events` (no `UPDATE/DELETE`),
and each row hashes the previous one so tampering is detectable by `incidentd verify-timeline`.

---

## 5. Lifecycle & severity

### 5.1 State machine

```mermaid
stateDiagram-v2
  [*] --> detected
  detected --> declared
  detected --> dismissed
  declared --> investigating
  declared --> resolved : false alarm after declare
  investigating --> identified
  investigating --> mitigating
  investigating --> resolved
  identified --> mitigating
  identified --> resolved
  mitigating --> monitoring
  mitigating --> investigating : mitigation failed
  monitoring --> resolved
  monitoring --> investigating : regression
  resolved --> postmortem
  resolved --> investigating : reopen (≤72h)
  resolved --> closed : SEV3/4 only, PM waived
  postmortem --> closed
  dismissed --> [*]
  closed --> [*]
```

Rules: every transition is a `TimelineEvent`; any other transition is rejected with the allowed list.
`resolved → closed` is blocked for SEV1/2 (postmortem mandatory). Reopen after 72 h creates a new linked incident.

### 5.2 Default severity matrix (SLO status × tenant tier)

| SLO status ↓ / tier → | enterprise | business | starter | unknown tier |
|---|---|---|---|---|
| `breached` | **SEV1** | **SEV2** | SEV3 | SEV2 (proposed, never auto) |
| `at_risk` | **SEV2** | SEV3 | SEV4 | SEV3 (proposed) |
| `timeout` / `no_data` (monitoring gap) | SEV3 | SEV4 | SEV4 | SEV4 |

Modifiers: ≥ 3 tenants on the same breached service → +1 level (max SEV1). Manual `sev` always wins and is logged.

**Auto-declare rule (Q6 = b):** auto-declare iff computed SEV ∈ {SEV1, SEV2} **and** tier is known **and** the
status persisted ≥ 2 consecutive Multiplexer evaluations (~2 min, configurable) **and** no open incident already
holds the same `dedupKey`. Otherwise the bot posts a *proposal* card (Declare / Dismiss) to the triage channel.
`timeout`/`no_data` never auto-declare.

**Company policy override (Q11):** `config/severity-policy.yaml` replaces the matrix, modifiers and auto-declare
thresholds; validated at startup against `schemas/severity-policy.schema.json`; hot-reloaded on SIGHUP; each load
writes its hash to the audit log.

---

## 6. Command surface (identical verbs on every platform)

Slack: `/incident <verb> …` · Teams: `@IncidentBot <verb> …` (plus Adaptive Card buttons). Same parser in `core/commands`.

| Verb | Syntax | Who |
|---|---|---|
| `declare` | `declare "<title>" [--sev N] [--service S]` | anyone |
| `sev` | `sev <1-4> [--reason "…"]` | IC |
| `role` | `role <ic\|comms\|ops\|scribe> @user` | IC (anyone may take IC if vacant) |
| `status` | `status [investigating\|identified\|mitigating\|monitoring]` — shows or transitions | IC |
| `update` | `update "<text>"` — stakeholder update, resets cadence timer | IC, comms |
| `triage` | `triage [--focus "…"]` | any participant |
| `run` | `run <actionId> [k=v …]` — creates an `ActionRequest`; never executes directly | participant (approval rules apply) |
| `resolve` | `resolve ["summary"]` | IC |
| `postmortem` | `postmortem [draft\|publish]` | IC |
| `timeline` | `timeline [--last N]` | anyone |

Status cadence: SEV1 30 m, SEV2 60 m, SEV3/4 none; the bot nudges the IC when overdue.

---

## 7. AI triage (Step 2)

### 7.1 Tools exposed to the model (read-only only)

| Tool | Port method | Shown when |
|---|---|---|
| `get_incident_context` | core (timeline, signals, services) | always |
| `get_slo_status` / `get_slo_events` | `SloSource` | SloSource configured |
| `query_metric` | `ObservabilityProvider.QueryMetric` | capability `metrics` |
| `search_logs` | `ObservabilityProvider.SearchLogs` | capability `logs` |
| `list_alerting_monitors` | `ObservabilityProvider.MonitorsInState` | capability `monitors` |
| `list_events` (deploys, config changes) | `ObservabilityProvider.Events` | capability `events` |
| `list_k8s_workload` / `k8s_events` / `rollout_history` / `pod_logs` | `CloudProvider` read methods | k8s adapter enabled, scope in allow-list |
| `who_is_on_call` | `PagingProvider.WhoIsOnCall` | always |
| `search_similar_incidents` | postmortem retrieval | ≥ 1 closed incident |

There is no write tool. `recommendedActions` are text proposals; turning one into an `ActionRequest` requires a human click.

### 7.2 Output contract (`schemas/triage-result.schema.json`)

```json
{
  "summary": "string ≤ 600 chars",
  "blastRadius": {"services": ["…"], "tenants": ["…"], "regions": ["…"], "userImpact": "string"},
  "hypotheses": [{
    "cause": "string",
    "confidence": 0.0,
    "evidence": [{"tool": "query_metric", "ref": "call_07", "finding": "p99 latency 4.1s from 10:42Z"}]
  }],
  "recommendedActions": [{"actionId": "k8s.rollout_undo", "params": {}, "rationale": "…", "risk": "high"}],
  "missingData": ["string"]
}
```

### 7.3 Guardrails

| Guardrail | Enforcement (in code, not prompt) |
|---|---|
| Budget | ≤ 15 tool calls, ≤ 90 s wall clock, ≤ 8 KB per tool result (truncated with marker); on exhaustion return partial result with `missingData` |
| Citations | Every `evidence.ref` must equal a tool-call ID from this run; hypotheses with zero valid refs are **dropped**; drop count logged |
| Prompt injection | Tool output wrapped as untrusted data in a delimited block; system prompt states it is data, not instructions; model has no write tools; actions restricted to catalog IDs regardless of output |
| Redaction | Go gateway redacts before returning: secrets (key/token patterns, `Authorization:` headers, AWS key IDs, JWTs, connection strings), emails → hashed tokens, IPs optional; per-adapter allow-list of fields |
| Action restriction | `recommendedActions[].actionId` ∉ catalog → removed; params validated against catalog schema; `risk` overwritten from catalog |
| Schema | Pydantic validation; one repair retry on invalid JSON; then fail closed with "triage unavailable" message |
| Confidence | Shown as Low/Med/High buckets, never a bare percentage, to avoid false precision at 3 a.m. |

### 7.4 Evaluation plan (Q12)

- **Corpus:** ≥ 20 synthetic incidents in `test/fixtures/incidents/` (recorded-shape tool responses + ground-truth
  root cause + "evidence that matters"). Labelled **`NOT REPRESENTATIVE`** in every report.
- **Historical (optional):** `brain eval import --since 90d` pulls PD incidents, Jira tickets and Slack war-room
  history; hard cap 90 days, redaction applied at import, stored only in the eval DB.
- **Metrics:** top-1 hypothesis accuracy, top-3 accuracy, evidence precision (cited refs judged relevant / all
  cited refs), uncited-hypothesis rate (must be 0 after filtering), non-catalog action rate (must be 0), p95 latency,
  cost per run.
- **Scoring:** cause match by a rubric (human-labelled; LLM-judge allowed only as a second rater, never sole).
- **Gate to enable remediation (see §10):** on ≥ 20 incidents of which ≥ 10 historical: top-1 ≥ 60 %, top-3 ≥ 80 %,
  evidence precision ≥ 90 %, p95 ≤ 90 s. Synthetic-only results cannot open the gate.

---

## 8. Remediation (Step 3) — human-in-the-loop

### 8.1 Catalog (`remediation/catalog/*.yaml`, version-controlled, CODEOWNERS = SRE leads)

```yaml
id: k8s.rollout_restart
provider: kubernetes
risk: low
params:                    # JSON Schema
  type: object
  required: [namespace, deployment]
  properties:
    namespace: {type: string, pattern: "^[a-z0-9-]{1,63}$"}
    deployment: {type: string, pattern: "^[a-z0-9-]{1,253}$"}
allowedScopes:
  clusters: [prod-eu-1, prod-us-1]
  namespaces: ["payments-*", "checkout"]
verify:
  type: rollout_status
  timeout: 5m
```

v1 catalog: `k8s.rollout_restart` (low) · `k8s.scale_deployment` (medium, bounded min/max) · `k8s.rollout_undo` (high) ·
`datadog.mute_monitor` (low, ≤ 2 h) · `pagerduty.escalate` (low).

### 8.2 Approval policy

| Risk | Required | Constraints |
|---|---|---|
| low | 1 approver | approver may be the requester only if they are the IC |
| medium | IC | IC ≠ requester |
| high | IC **and** service owner | two distinct people, neither is the requester; service owner from Multiplexer `services.owner` → email → chat user |

Approvals expire after **15 min** (configurable); expiry is a timeline event. Re-approval required if params change.
Approver identity = verified chat user → email → must match an active PD/directory user.

### 8.3 Flow

```mermaid
sequenceDiagram
  participant H as Human (chat)
  participant C as incidentd core
  participant R as remediation
  participant K as CloudProvider
  H->>C: run k8s.rollout_restart ns=checkout deploy=api
  C->>R: validate (catalog, schema, scope, kill switch)
  R->>K: DryRun → diff
  R->>C: ActionRequest(pending) + diff
  C->>H: approval card (diff, risk, required approvers, expiry)
  H->>C: Approve (IC)
  C->>R: policy check (distinct people, not expired)
  R->>K: Execute  (v1: ErrExecutorDisabled → "dry-run only")
  R->>K: Verify step
  R->>C: result → timeline + Jira comment
```

### 8.4 Controls

- **Kill switch:** env `REMEDIATION_ENABLED=false` (default in v1) **and** runtime DB flag toggled by `admin` role; both must be true.
- **No free-form shell** from chat, ever. Unknown `actionId` is rejected.
- **K8s RBAC:** v1 ServiceAccount gets `get, list, watch` on `pods, pods/log, events, deployments, replicasets` only.
  The write Role (`patch` on `deployments` for restart/undo, `update` on `deployments/scale`) ships as a separate
  manifest, **not applied in v1**, namespace-scoped to catalog `allowedScopes`.
- Invariant (tested): no code path calls `CloudProvider.Execute` without an `ActionRequest` whose approvals satisfy policy **and** are recorded in the timeline first.

---

## 9. Postmortem & learning (Step 4)

Draft sections: summary · impact (services, tenants, duration, **error budget burned** from `SloSource.BudgetBurned`
with quality flag) · detection · timeline (from timeline events) · root cause · contributing factors · what went well /
badly · action items (→ Jira). Blameless language check: drafter is instructed and a linter flags person-blaming phrasing.

| Metric | Definition |
|---|---|
| TTD | `detectedAt − impactStartedAt` (impact start = earliest attached signal's onset, editable by IC) |
| TTA | `acknowledgedAt − detectedAt` (first PD ack or IC assignment) |
| Time-to-declare | `declaredAt − detectedAt` |
| TTM | `mitigatedAt − impactStartedAt` (first entry to `monitoring`) |
| TTR | `resolvedAt − impactStartedAt` |
| Budget burned | `SloSource.BudgetBurned(slo, impactStart, resolvedAt)`; flagged `stale` until Multiplexer gap G4 is fixed |

Similar-incident retrieval: v1 **BM25** (`rank-bm25`) over title, summary, root cause, services, top log signatures;
v2 pgvector embeddings with hybrid ranking.

---

## 10. Security model

| Area | Control |
|---|---|
| Slack inbound | Signing-secret HMAC-SHA256 over `v0:{timestamp}:{body}`, header `X-Slack-Signature`, reject if `X-Slack-Request-Timestamp` skew > 5 min — **UNVERIFIED** (from memory; api.slack.com blocked) |
| Teams inbound | Bot Framework JWT: validate against OpenID metadata/JWKS, issuer, audience = app ID, `serviceUrl` claim — **UNVERIFIED** (learn.microsoft.com blocked) |
| PagerDuty inbound | Webhooks v3 HMAC-SHA256 signature header `X-PagerDuty-Signature` (`v1=…`) — **UNVERIFIED** |
| Multiplexer | Bot uses a scoped service token (`views:read`) once PR G2 lands; until then only the unauthenticated guardrail GETs are usable |
| Identity | chat user → profile email → PD user / Azure AD UPN; unmapped users can read and `declare` but cannot approve |
| Credentials | One credential per adapter, least privilege (PD: read + incident write; Jira: one project; Datadog: read-only app key scopes; K8s: read-only SA); loaded from mounted secrets (K8s Secret / Vault agent), never env-logged, never in prompts |
| LLM data | Redaction in Go gateway; no raw payloads to the LLM; Anthropic API over TLS; Bedrock/Vertex adapters require private endpoints |
| Audit | Hash-chained append-only timeline; structured logs with `incident_id` + `correlation_id`; approvals record who/when/what diff |
| Service-to-service | mTLS between `incidentd` and `brain`; `brain` token scoped to tool gateway read endpoints |

---

## 11. Roadmap & exit gates

| Phase | Scope | Exit gate (measurable) |
|---|---|---|
| P0 Foundations | Repo, CI (Go vet/test, ruff/mypy/pytest, schema drift check), config, migrations, health | CI green; config fails fast on invalid input |
| P1 Declare & coordinate | Slack: all 10 verbs, war room, roles, PD paging, Jira ticket, cadence, timeline; Multiplexer + PD ingest; auto-declare SEV1/2; Teams adapter behind flag | Game-day: declare→resolve end-to-end on Slack; 100 % of commands produce timeline events; signature-verification tests pass on recorded fixtures; duplicate PD+Multiplexer signal → 1 incident |
| P2 Triage | Tool gateway, agent loop, schema, guardrails, eval harness | §7.4 metrics reported on ≥ 20 incidents; 0 uncited hypotheses; 0 non-catalog actions |
| P3 Remediation (dry-run) | Catalog, approval engine, approval cards, dry-run diffs | Approval-policy tests incl. two-person + expiry; invariant test (§8.4) |
| P3b Remediation live (low risk) | Apply write RBAC, flip kill switch for `low` only | **Only after P2 gate passes with ≥ 10 historical incidents** + security review sign-off |
| P4 Postmortem | Drafter, metrics, action items → Jira, BM25 | Metrics match hand-computed values on fixtures; IC accepts draft with edits on ≥ 3 drill incidents |
| v2 | Teams GA, AWS adapter, pgvector, Bedrock/Vertex, Multiplexer outbound webhook, Datadog monitor webhook ingest | — |

---

## 12. Multiplexer gap PRs (against `main`, each noting the missing `dev` branch)

| ID | Gap (file) | PR | Tests |
|---|---|---|---|
| G1 | `/api/slo-guardrails/**` has no `requirePermission` on any method (`src/routes/api/slo-guardrails/**`) | Add `views:read` / `views:write` checks | Handler tests: 401/403/200 |
| G2 | Cookie-session-only auth; no machine identity (`src/lib/server/auth/middleware.ts`) | Hashed, scoped, expiring service tokens via `Authorization: Bearer`; CSRF skipped only for bearer-authenticated requests | Token validation, scope, expiry, CSRF bypass only with bearer |
| G3 | Guardrail → tenant requires 3 joins; not exposed | Embed `tenant {id, slug, tier}` in guardrail GET responses | Response-shape tests |
| G4 | Current burn rate / budget only in events written on status change (`slo-guardrails.ts`) | Persist last computed metrics on `slo_guardrails` (migration V10) | Evaluator test asserts persistence each cycle |
| G5 (v2) | No generic outbound webhook | Signed webhook dispatcher on guardrail transitions | Signature + retry tests |

Note: Multiplexer's budget math weights `yellow` at 0.5 and counts `no_data` as bad (`slo-guardrails.ts:93`).
Postmortem figures will differ from Datadog SLOs; documented, not changed.

---

## 13. Risks

| # | Risk | Mitigation |
|---|---|---|
| R1 | Vendor details marked UNVERIFIED are wrong | Verify against docs before P1 code freeze (needs doc hosts allowed, or you confirm); recorded-fixture tests per adapter |
| R2 | **No maintained Go SDK for Teams.** `msbotbuilder-go` last released 2021-10. Microsoft 365 Agents SDK exists for Python (`microsoft-agents-hosting-teams` 1.9.0) | Implement the Bot Framework REST protocol directly in Go (JWT validation + POST to `serviceUrl`), small surface; fallback: Teams adapter as a thin Python sidecar using the Agents SDK. Open question Q2 |
| R3 | Two languages = two toolchains | Strict boundary (§2.1), generated contracts, single Helm chart, one CI pipeline |
| R4 | Auto-declare noise from flapping SLOs | 2-evaluation persistence, dedupe on `dedupKey`, SEV1/2 only, weekly review of auto-declares |
| R5 | Budget burned is stale until G4 | `quality` flag shown in postmortem |
| R6 | LLM hallucinated causes | Citation enforcement in code; confidence buckets; remediation gated on eval |

## 14. Open questions

- **Q1** Agent loop in Python (proposed) or all-Go? Proposal keeps vendor access in Go either way.
- **Q2** Teams: Go REST implementation (proposed) or Python sidecar?
- **Q3** Postgres: dedicated instance for the bot, or a schema on an existing cluster?
- **Q4** War-room channels: public by default, private for security incidents (`--private`)?
- **Q5** Which K8s clusters/contexts and namespaces are in scope for read access?
- **Q6** Jira project key and issue type for incident tickets and action items?
- **Q7** Allow doc hosts (`api.slack.com`, `developer.pagerduty.com`, `learn.microsoft.com`, `developer.atlassian.com`) in this session's network settings so I can clear the UNVERIFIED items?

---

## Appendix A — Verification log

| Item | Status | Source |
|---|---|---|
| Multiplexer endpoints, types, auth, PD dedup key, tenant tiers | Verified | Source code, `main@e14d5ca` |
| Multiplexer `dev` branch | **Absent** (only `main` + 4 feature branches) | `git branch -a` |
| Datadog `POST /api/v2/logs/events/search` (regional hosts) | Verified | docs.datadoghq.com/api/latest/logs |
| Datadog metrics query, monitors, events endpoints | UNVERIFIED | not yet fetched |
| Slack signing, Events API, Block Kit, `conversations.create` | UNVERIFIED | host blocked |
| PagerDuty REST v2, Webhooks v3 signatures | UNVERIFIED | host blocked |
| Teams Bot Framework auth | UNVERIFIED | host blocked |
| Jira Cloud REST v3 | UNVERIFIED | not yet fetched |
| Go SDKs exist: `slack-go/slack` v0.35.0, `PagerDuty/go-pagerduty` v1.8.0, `DataDog/datadog-api-client-go/v2` v2.67.0, `k8s.io/client-go` v0.37.2, `andygrunwald/go-jira` v1.17.0, `jackc/pgx/v5` v5.12.0 | Verified | proxy.golang.org |
| Python packages: `anthropic` 2.7.0, `rank-bm25` 0.2.2, `pydantic` 2.14.3, `slack-bolt` 1.30.0, `microsoft-agents-hosting-teams` 1.9.0 | Verified | PyPI |
| Toolchain in build env: Go 1.24.7, Python 3.13.18 | Verified | local |
