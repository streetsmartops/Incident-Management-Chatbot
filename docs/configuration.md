# Configuration reference

`incidentd` reads one YAML file (`-config`, default `$INCIDENTD_CONFIG` or `config/config.yaml`).

- **Unknown keys are rejected.**
- **Secrets are never accepted in YAML.** They come from env `NAME` or from the file at `NAME_FILE`.
- Startup validates everything and reports **all** problems at once.
- Every integration is off until `enabled: true`.

Annotated example: [`config/example.yaml`](../config/example.yaml).

## Secrets and environment

| Name | Required when | Notes |
|---|---|---|
| `DATABASE_URL` | `database.driver: postgres` | `postgres://user:pass@host:5432/db?sslmode=require` |
| `SLACK_BOT_TOKEN` | `slack.enabled` | `xoxb-…` |
| `SLACK_SIGNING_SECRET` | `slack.enabled` | Verifies every Slack request |
| `TEAMS_APP_PASSWORD` | `teams.enabled` | Azure Bot client secret |
| `PAGERDUTY_API_TOKEN` | `pagerduty.enabled` | REST API key, not read-only |
| `PAGERDUTY_WEBHOOK_SECRET` | `pagerduty.enabled` | Webhooks v3 signing secret |
| `DATADOG_API_KEY`, `DATADOG_APP_KEY` | `datadog.enabled` | Application key scoped to read |
| `JIRA_API_TOKEN` | `jira.enabled` | Paired with `jira.email` |
| `MULTIPLEXER_TOKEN` | optional | `mpx_…` service token with `views:read`. Without it, tier is unknown (no auto-declare) |
| `ANTHROPIC_API_KEY` | `llm.enabled` with `provider: anthropic` | |
| `INCIDENTD_ADMIN_TOKEN` | to use `/admin/*` | Random value; admin endpoints are disabled if empty |
| `REMEDIATION_ENABLED` | | Env half of the remediation kill switch; `true` to allow. Default off |
| `INCIDENTD_CONFIG` | | Default config path |

Every secret also accepts `NAME_FILE=/path/to/file` (trailing whitespace trimmed).

## `server`

| Key | Default | Meaning |
|---|---|---|
| `listen` | `:8080` | Public HTTP (chat, webhooks) and internal routes (`/healthz`, `/readyz`, `/admin/*`) |
| `metricsListen` | `:9090` | Prometheus `/metrics`. Keep it internal. `""` disables it |

`logLevel`: `debug` \| `info` (default) \| `warn` \| `error`.

## `database`

| Key | Default | Meaning |
|---|---|---|
| `driver` | `postgres` | `postgres`, or `memory` (local development only; nothing persists) |
| `schema` | `incident_bot` | Dedicated schema; `^[a-z_][a-z0-9_]*$` |

## `incident`

| Key | Default | Meaning |
|---|---|---|
| `defaultPlatform` | `slack` | Platform for auto-declared incidents and proposals (`slack` \| `teams`) |
| `warRoomPrefix` | `inc-` | War room name: `<prefix><year>-<n>-<title-slug>` |
| `privateByDefault` | `false` | War rooms are public unless `declare --private` |
| `pageAtOrAbove` | `2` | Page PagerDuty for this severity or worse (`0` = never) |
| `ticketAtOrAbove` | `3` | File a Jira ticket for this severity or worse (`0` = never) |
| `statusCadence` | `{1: 30m, 2: 60m}` | Nudge the IC when no `update` has been posted for this long |
| `admins` | `[]` | Emails allowed to act as IC on any incident |

`severityPolicyFile`: path to a company policy (see `config/severity-policy.example.yaml`). When empty,
the built-in matrix is used:

| SLO status \ tier | enterprise | business | starter | unknown |
|---|---|---|---|---|
| breached | SEV1 | SEV2 | SEV3 | SEV2 |
| at_risk | SEV2 | SEV3 | SEV4 | SEV3 |
| timeout / no_data | SEV3 | SEV4 | SEV4 | SEV4 |

Severity rises one level when 3 or more tenants have a non-ok SLO on the same service. Only SEV1 and SEV2
auto-declare, and only when the tier is known and the status persists for 2 evaluations; everything else
is proposed. After an incident resolves, the same breach is not re-declared until the SLO recovers.

## `slack`

| Key | Meaning |
|---|---|
| `enabled` | Turn on Slack |
| `announceChannelId` | Channel for announcements and proposals (`C…`) |
| `apiUrl` | Override `https://slack.com/api/` (tests) |

## `teams` (optional)

| Key | Meaning |
|---|---|
| `enabled` | Turn on Teams |
| `appId` | Azure Bot Microsoft App ID (also the JWT audience) |
| `tenantId` | Single-tenant bots; empty means multi-tenant (`botframework.com` token endpoint) |
| `announceConversationId`, `announceServiceUrl` | Where announcements and proposals go; both are logged as `teams activity` when the bot receives a message there |

## `pagerduty`

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | |
| `apiUrl` | `https://api.pagerduty.com` | |
| `fromEmail` | | PagerDuty user email sent as `From` on writes (required) |
| `services` | `{}` | `our-service-name: PXXXXXX` |
| `defaultService` | | Service ID used when no mapping matches |

## `datadog`

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | |
| `site` | `datadoghq.com` | Determines `https://api.<site>` |
| `apiUrl` | | Override the base URL |

## `jira`

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | |
| `baseUrl` | | `https://<org>.atlassian.net` |
| `email` | | Account email paired with `JIRA_API_TOKEN` |
| `projectKey` | | e.g. `OPS` |
| `issueType` | `Incident` | Must exist in the project |
| `actionItemIssueType` | `Task` | For postmortem action items |

## `kubernetes`

| Key | Meaning |
|---|---|
| `enabled` | Read-only triage tools and dry-run diffs |
| `clusters[].name` | Name used in tools and catalog params (`cluster=`) |
| `clusters[].inCluster` | Use the pod's ServiceAccount |
| `clusters[].kubeconfigPath`, `context` | For other clusters |
| `clusters[].namespaces` | **Required** allow-list; globs like `payments-*` |

## `multiplexer`

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | |
| `baseUrl` | | Multiplexer URL |
| `pollInterval` | `60s` | Minimum `10s` |

## `llm`

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Enables `/incident triage` and LLM-drafted postmortem narrative |
| `provider` | `anthropic` | `bedrock` and `vertex` are stubs (planned) |
| `model` | `claude-opus-5-5` | |
| `effort` | `high` | `low` \| `medium` \| `high` \| `xhigh` \| `max` |
| `maxTokens` | `16000` | Per model response |
| `fallbacks` | `default` | Server-side refusal fallback; `off` to disable |
| `baseUrl` | | Egress gateway or private endpoint |
| `triage.maxToolCalls` | `15` | Tool budget per run |
| `triage.timeout` | `90s` | Wall-clock budget |
| `triage.maxToolOutputBytes` | `8192` | Per tool result, after redaction |
| `triage.lookbackWindow` | `2h` | Query window before impact start |

## `redaction`

| Key | Default | Meaning |
|---|---|---|
| `hashEmails` | `true` | Replace emails with `user-<hash>` before LLM calls |
| `extraPatterns` | `[]` | Extra regexes replaced with `[REDACTED:custom]` |

## `remediation`

| Key | Default | Meaning |
|---|---|---|
| `catalogDir` | `remediation/catalog` | In the image: `/app/remediation/catalog` |
| `approvalTTL` | `15m` | Approval requests expire after this |
| `serviceOwners` | `{}` | `service: owner@example.com`, the second approver for high-risk actions. Falls back to Multiplexer `services.owner` |
