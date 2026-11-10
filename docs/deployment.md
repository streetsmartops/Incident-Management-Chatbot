# Deployment guide

Step-by-step production deployment of `incidentd` on Kubernetes. Allow half a day
for a first install. Most of that is creating credentials in each vendor tool.

![Deployment topology](diagrams/deployment.svg)

## Contents

1. [Checklist](#1-checklist)
2. [Prerequisites](#2-prerequisites)
3. [Build and publish the image](#3-build-and-publish-the-image)
4. [PostgreSQL](#4-postgresql)
5. [Slack app](#5-slack-app)
6. [Microsoft Teams (optional)](#6-microsoft-teams-optional)
7. [PagerDuty](#7-pagerduty)
8. [Datadog](#8-datadog)
9. [Jira Cloud](#9-jira-cloud)
10. [Multiplexer](#10-multiplexer)
11. [Kubernetes read access](#11-kubernetes-read-access)
12. [Anthropic API](#12-anthropic-api)
13. [Configuration](#13-configuration)
14. [Secrets](#14-secrets)
15. [Apply the manifests](#15-apply-the-manifests)
16. [Verify the deployment](#16-verify-the-deployment)
17. [Monitoring and dashboards](#17-monitoring-and-dashboards)
18. [Upgrades and rollback](#18-upgrades-and-rollback)
19. [Enabling remediation (later phase)](#19-enabling-remediation-later-phase)
20. [Troubleshooting the install](#20-troubleshooting-the-install)

---

## 1. Checklist

| # | Item | Owner | Done |
|---|---|---|---|
| 1 | Kubernetes namespace, ingress with TLS, egress allowed to vendor APIs | Platform | ☐ |
| 2 | Container image built and pushed | Platform | ☐ |
| 3 | Postgres database, roles and schema | DBA | ☐ |
| 4 | Slack app created, installed, bot invited to announce channel | Slack admin | ☐ |
| 5 | (Optional) Azure Bot + Teams app | M365 admin | ☐ |
| 6 | PagerDuty REST token, From user, service map, v3 webhook | PagerDuty admin | ☐ |
| 7 | Datadog API key + read-scoped application key | Datadog admin | ☐ |
| 8 | Jira service account API token, project and issue types | Jira admin | ☐ |
| 9 | Multiplexer PRs merged, service token created | Multiplexer owner | ☐ |
| 10 | Read-only RBAC in each cluster/namespace triage may read | Platform | ☐ |
| 11 | Anthropic API key | Platform | ☐ |
| 12 | ConfigMap and Secret populated, `config-check` passes | Platform | ☐ |
| 13 | Prometheus/Grafana or Datadog dashboards and alerts | SRE | ☐ |
| 14 | Smoke tests in section 16 pass | SRE | ☐ |

You can start with only Slack and Postgres. Every other integration is off until enabled,
so you can add them one at a time.

## 2. Prerequisites

| Requirement | Notes |
|---|---|
| Kubernetes 1.27+ | Any conformant cluster. Two replicas recommended. |
| Ingress controller with TLS | Slack, Teams and PagerDuty must reach the public HTTPS endpoint. |
| PostgreSQL 16 | Managed (RDS, Cloud SQL, Azure Database) or self-hosted. ~1 GB is plenty for years of incidents. |
| Egress | HTTPS to `slack.com`, `api.pagerduty.com`, `api.<datadog site>`, `<org>.atlassian.net`, `api.anthropic.com`, your Multiplexer URL, Kubernetes API servers, and for Teams `login.microsoftonline.com`, `login.botframework.com`, `smba.trafficmanager.net`. |
| Secret manager | External Secrets, Vault agent, sealed-secrets, or plain Kubernetes Secrets. |
| Container registry | Any registry your cluster can pull from. |
| Go 1.26+ / Docker | Only to build the image. |

## 3. Build and publish the image

```bash
docker build --build-arg VERSION=v1.0.0 -t registry.example.com/platform/incidentd:v1.0.0 .
```

```bash
docker push registry.example.com/platform/incidentd:v1.0.0
```

The image is distroless (`gcr.io/distroless/static-debian12:nonroot`) and runs as non-root with a
read-only root filesystem. It contains the binary, the remediation catalog
(`/app/remediation/catalog`) and the example config. `VERSION` appears in the
`incidentd_build_info` metric.

In production, pin the image by digest in `deploy/k8s/deployment.yaml`.

## 4. PostgreSQL

`incidentd` uses a **dedicated schema** (default `incident_bot`) and migrates it at startup
under an advisory lock, so several replicas can start at once.

```sql
-- as a DBA
CREATE DATABASE incidentd;
\c incidentd
CREATE ROLE incidentd_owner LOGIN PASSWORD '…';
CREATE ROLE incidentd_app   LOGIN PASSWORD '…';
CREATE SCHEMA incident_bot AUTHORIZATION incidentd_owner;
GRANT USAGE ON SCHEMA incident_bot TO incidentd_app;
```

There are two options for running migrations:

- **Simple:** the app connects as `incidentd_owner` and migrates itself.
- **Least privilege:** run once as `incidentd_owner` (for example as a Kubernetes Job using the same image and config), then run the app as `incidentd_app` with:

  ```sql
  GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA incident_bot TO incidentd_app;
  REVOKE UPDATE, DELETE, TRUNCATE ON incident_bot.timeline_events FROM incidentd_app;
  ALTER DEFAULT PRIVILEGES FOR ROLE incidentd_owner IN SCHEMA incident_bot
        GRANT SELECT, INSERT, UPDATE ON TABLES TO incidentd_app;
  ```

The `timeline_events` table also has triggers that block `UPDATE`, `DELETE` and `TRUNCATE`
for every role, including the owner. To delete an incident's data you must drop the schema.

DSN (the `DATABASE_URL` secret):

```
postgres://incidentd_app:PASSWORD@db.internal:5432/incidentd?sslmode=require
```

Backups: back up the database like any other system of record. The timeline is your audit trail.

## 5. Slack app

1. Edit `deploy/slack/manifest.yaml` and replace `https://incidentd.example.com` with your public hostname (two places).
2. Go to <https://api.slack.com/apps>, then **Create New App**, then **From an app manifest**, choose the workspace, and paste the manifest.
3. **Install to Workspace**.
4. Copy the **Bot User OAuth Token** (`xoxb-…`) into the `SLACK_BOT_TOKEN` secret.
5. Copy **Basic Information, then Signing Secret** into the `SLACK_SIGNING_SECRET` secret.
6. Create or choose the announce channel (for example `#incidents`) and `/invite @incidentbot`.
7. Get the channel ID: open the channel, then **channel name, then About**. The ID (`C0…`) is at the bottom. Set it as `slack.announceChannelId`.

The manifest requests these bot scopes, each used by the adapter:

| Scope | Used for |
|---|---|
| `commands` | `/incident` |
| `chat:write` | messages, updates |
| `channels:manage`, `groups:write` | create public/private war rooms |
| `channels:write.invites` | invite IC and on-call |
| `channels:write.topic` | war room topic (key, severity, state) |
| `users:read`, `users:read.email` | map Slack users to emails (identity) |

`should_escape: true` on the command is required so @-mentions resolve to user IDs.

## 6. Microsoft Teams (optional)

Teams runs alongside Slack. Bots cannot create channels without Microsoft Graph, so on
Teams incidents run in the announce conversation and commands take `--incident INC-…`.

1. In the Azure portal, create an **Azure Bot**. It can be multi-tenant; for single-tenant, note the tenant ID.
2. **Configuration, then Messaging endpoint:** `https://<host>/teams/messages`.
3. **Microsoft App ID:** goes into `teams.appId`. Create a client secret and put it in `TEAMS_APP_PASSWORD`. For single-tenant bots, set `teams.tenantId`.
4. **Channels:** add **Microsoft Teams**.
5. Fill in `deploy/teams/manifest.json` (`id` and `bots[0].botId` = app ID, plus hostnames), add `color.png` (192×192) and `outline.png` (32×32), zip the three files, and upload them in Teams Admin Center or **Apps, then Manage your apps, then Upload**.
6. Add the app to the incidents channel and send `@IncidentBot help`. From the log line `teams activity` (or a temporary debug log), take the conversation ID and `serviceUrl`, and set `teams.announceConversationId` and `teams.announceServiceUrl`.
7. Set `teams.enabled: true`.

## 7. PagerDuty

1. **API token:** go to **Integrations, then API Access Keys, then Create New API Key**. It must not be read-only. Put it in `PAGERDUTY_API_TOKEN`.
2. **From user:** pick a PagerDuty user (ideally a service user) whose email goes into `pagerduty.fromEmail`. PagerDuty requires this `From` header for writes (create, resolve, notes).
3. **Service map:** for each service name you use in incidents (`--service checkout`, Multiplexer service names), find the PagerDuty service ID in the URL (`/service-directory/PXXXXXX`):

   ```yaml
   pagerduty:
     services: {checkout: PXXXXXX, payments: PYYYYYY}
     defaultService: PZZZZZZ   # used when no mapping matches
   ```

4. **Webhook:** go to **Integrations, then Generic Webhooks (v3), then New Webhook**:
   - URL: `https://<host>/webhooks/pagerduty`
   - Scope: the account, or the services above
   - Events: the incident events (triggered, acknowledged, resolved, priority updated)
   - Copy the **signing secret** into `PAGERDUTY_WEBHOOK_SECRET`.

Why the webhook matters:
- When the bot pages, the on-call's acknowledgement comes back through the webhook. That sets the incident's acknowledged time, which drives TTA.
- Pages that Multiplexer opens (titled `SLO Breach: <name>`) are correlated to the same incident, so a breach never creates two incidents.

## 8. Datadog

1. **API key:** go to **Organization Settings, then API Keys**. Put it in `DATADOG_API_KEY`.
2. **Application key:** go to **Organization Settings, then Application Keys**, create a key owned by a service account, and **scope it to read** (metrics, logs, monitors, events). Put it in `DATADOG_APP_KEY`. Check the scope names in your org's key UI.
3. Set `datadog.site` to your site (`datadoghq.com`, `datadoghq.eu`, `us3.datadoghq.com`, `us5.datadoghq.com`, `ap1.datadoghq.com`, …) and `datadog.enabled: true`.

The adapter only calls read endpoints: `GET /api/v1/query`, `POST /api/v2/logs/events/search`,
`GET /api/v1/monitor` and `GET /api/v2/events`.

## 9. Jira Cloud

1. Create or choose a service account. Create an API token at <https://id.atlassian.com/manage-profile/security/api-tokens> and put it in `JIRA_API_TOKEN`.
2. Give the account create/comment permission in **one** project only.
3. Configure:

   ```yaml
   jira:
     enabled: true
     baseUrl: https://your-org.atlassian.net
     email: incident-bot@example.com
     projectKey: OPS
     issueType: Incident          # must exist in the project
     actionItemIssueType: Task
   ```

By default, tickets are filed for SEV1–SEV3 (`incident.ticketAtOrAbove: 3`).

## 10. Multiplexer

1. Merge the Multiplexer PRs in this order:
   - [#7](https://github.com/streetsmartops/Multi-slo-observability-app/pull/7): V9 migration fix
   - [#6](https://github.com/streetsmartops/Multi-slo-observability-app/pull/6): service tokens
   - [#9](https://github.com/streetsmartops/Multi-slo-observability-app/pull/9): latest metrics

   [#5](https://github.com/streetsmartops/Multi-slo-observability-app/pull/5) (auth) and [#8](https://github.com/streetsmartops/Multi-slo-observability-app/pull/8) (tenant embedding) can be merged in any order.
2. As a Multiplexer admin, create a read-only token:

   ```bash
   curl -X POST https://multiplexer.internal/api/admin/service-tokens \
     -H 'Content-Type: application/json' -H "X-CSRF-Token: $CSRF" --cookie "$SESSION_COOKIE" \
     -d '{"name":"incident-bot","scopes":["views:read"],"expires_in_days":90}'
   ```

   Put the returned `mpx_…` token in `MULTIPLEXER_TOKEN`, and add a calendar reminder to rotate it before it expires.
3. Configure `multiplexer.enabled: true`, `multiplexer.baseUrl`, and optionally `pollInterval` (default 60s).

Without the token, tenant tier is `unknown`, so breaches are **proposed, never auto-declared**.

## 11. Kubernetes read access

Triage reads deployments, replica sets, pods, pod logs and events. Grant this **per namespace**
in every cluster the bot may read.

**Same cluster as the bot:**

```bash
kubectl apply -f deploy/k8s/namespace.yaml -f deploy/k8s/rbac-readonly.yaml
```

Copy the `RoleBinding` in `rbac-readonly.yaml` once per allowed namespace, then configure:

```yaml
kubernetes:
  enabled: true
  clusters:
    - name: prod-eu-1
      inCluster: true
      namespaces: ["checkout", "payments-*"]
```

**Other clusters:** create the same ClusterRole and RoleBindings there, bound to a
ServiceAccount in that cluster. Mint a token for it and build a kubeconfig. Mount the kubeconfig
from a Secret and reference it:

```yaml
    - name: prod-us-1
      kubeconfigPath: /etc/incidentd/kubeconfig/prod-us-1
      context: prod-us-1
      namespaces: ["checkout"]
```

The namespace allow-list is enforced in the bot as well as by RBAC. Do **not** apply
`deploy/k8s/rbac-remediation.yaml` yet (see [section 19](#19-enabling-remediation-later-phase)).

## 12. Anthropic API

1. Create an API key in the Anthropic Console (a separate workspace for this bot is recommended) and put it in `ANTHROPIC_API_KEY`.
2. Configure:

   ```yaml
   llm:
     enabled: true
     provider: anthropic
     model: claude-opus-5-5
     effort: high
     fallbacks: default      # server-side refusal fallback; "off" to disable
     baseUrl: ""            # optional egress gateway / private endpoint
   ```

What the model sees: only redacted tool output (tokens, keys, JWTs, passwords and URL credentials are removed; emails are hashed) and the incident timeline. Triage tools are read-only. See [security.md](security.md).

## 13. Configuration

Start from `config/example.yaml`; every key is documented in [configuration.md](configuration.md).
Put the file in the `incidentd-config` ConfigMap in `deploy/k8s/deployment.yaml`. A minimal
production config looks like this:

```yaml
server: {listen: ":8080", metricsListen: ":9090"}
database: {driver: postgres, schema: incident_bot}
incident:
  defaultPlatform: slack
  pageAtOrAbove: 2
  ticketAtOrAbove: 3
  admins: [sre-lead@example.com]
slack: {enabled: true, announceChannelId: C0123456789}
pagerduty: {enabled: true, fromEmail: incident-bot@example.com, services: {checkout: PXXXXXX}, defaultService: PZZZZZZ}
datadog: {enabled: true, site: datadoghq.com}
jira: {enabled: true, baseUrl: "https://your-org.atlassian.net", email: incident-bot@example.com, projectKey: OPS}
multiplexer: {enabled: true, baseUrl: "https://multiplexer.internal"}
kubernetes: {enabled: true, clusters: [{name: prod-eu-1, inCluster: true, namespaces: [checkout]}]}
llm: {enabled: true}
remediation: {catalogDir: /app/remediation/catalog, serviceOwners: {checkout: checkout-owner@example.com}}
```

Optional company severity policy: copy `config/severity-policy.example.yaml`, mount it, and set
`severityPolicyFile`.

## 14. Secrets

Secrets are never read from YAML. Each one is read from env `NAME` or from the file at
`NAME_FILE`. The deployment mounts the `incidentd-secrets` Secret as files.

With plain kubectl:

```bash
kubectl -n incidentd create secret generic incidentd-secrets \
  --from-literal=DATABASE_URL='postgres://…' \
  --from-literal=SLACK_BOT_TOKEN='xoxb-…' \
  --from-literal=SLACK_SIGNING_SECRET='…' \
  --from-literal=PAGERDUTY_API_TOKEN='…' \
  --from-literal=PAGERDUTY_WEBHOOK_SECRET='…' \
  --from-literal=DATADOG_API_KEY='…' \
  --from-literal=DATADOG_APP_KEY='…' \
  --from-literal=JIRA_API_TOKEN='…' \
  --from-literal=MULTIPLEXER_TOKEN='mpx_…' \
  --from-literal=ANTHROPIC_API_KEY='…' \
  --from-literal=TEAMS_APP_PASSWORD='' \
  --from-literal=INCIDENTD_ADMIN_TOKEN="$(openssl rand -hex 32)"
```

With External Secrets, map the same keys from your vault into a Secret named `incidentd-secrets`.
Every key the deployment references must exist; use an empty value for unused integrations.

## 15. Apply the manifests

```bash
kubectl apply -f deploy/k8s/namespace.yaml
```

```bash
kubectl apply -f deploy/k8s/rbac-readonly.yaml
```

```bash
kubectl apply -f deploy/k8s/deployment.yaml
```

```bash
kubectl apply -f deploy/k8s/ingress.yaml -f deploy/k8s/networkpolicy.yaml
```

Before applying, edit `deployment.yaml`: the image (pin a digest), the ConfigMap content and the
replica count. In `ingress.yaml`, set the host, ingress class and TLS issuer. In `networkpolicy.yaml`,
set the namespace labels of your ingress controller and monitoring stack.

The Ingress exposes **only** `/slack/commands`, `/slack/interactivity`, `/teams/messages` and
`/webhooks/pagerduty`. `/admin/*`, `/healthz`, `/readyz` and the metrics port stay inside the
cluster.

## 16. Verify the deployment

Run these in order.

1. **Pods ready:**

   ```bash
   kubectl -n incidentd get pods
   ```

   Both should be `Running` and `Ready`. `/readyz` checks the database.
2. **Config valid:**

   ```bash
   kubectl -n incidentd exec deploy/incidentd -- /app/incidentd config-check -config /etc/incidentd/config.yaml
   ```

   Expected output: `configuration OK`.
3. **Startup logs:**

   ```bash
   kubectl -n incidentd logs deploy/incidentd | head
   ```

   Look for `severity policy loaded`, `remediation … env_enabled=false`, `metrics listening` and `incidentd listening`.
4. **Slack:** in any channel, `/incident help` returns the command list.
5. **Signature check:** `curl -X POST https://<host>/slack/commands -d text=help` returns **401**.
6. **Test incident:** in a test channel:

   ```
   /incident declare "Deployment smoke test" --sev 4
   ```

   That should create a war room and timeline. No page is sent (SEV4 is below `pageAtOrAbove`).

   Then run `status investigating`, `resolve "test"` and `timeline`.
7. **Audit chain:**

   ```bash
   kubectl -n incidentd port-forward deploy/incidentd 8080 &
   ```

   ```bash
   curl -H "Authorization: Bearer $ADMIN" "localhost:8080/admin/timeline/verify?incident=INC-2026-0001"
   ```

   Expected: `"intact":true`.
8. **PagerDuty:** declare a SEV2 against a test service and check that the page arrives. Acknowledge it in PagerDuty; the timeline should gain `acknowledged`.
9. **Triage:** in that incident, run `/incident triage`. Results appear within 90 seconds and list the tools used.
10. **Multiplexer:** watch for `slo signal` log lines each poll cycle when a guardrail is not `ok`.
11. **Metrics:**

    ```bash
    kubectl -n incidentd port-forward svc/incidentd-metrics 9090 &
    ```

    ```bash
    curl -s localhost:9090/metrics | grep incidentd_build_info
    ```

## 17. Monitoring and dashboards

See [dashboards.md](dashboards.md) for Prometheus Operator, plain Prometheus, Grafana import
and Datadog setup, and [operations.md](operations.md) for the alert runbooks.

## 18. Upgrades and rollback

- **Upgrade:**
  1. Build a new tag and update the image in the Deployment. The rolling update keeps one replica serving.
  2. Migrations run on start; they are forward-only and run under an advisory lock.
- **Rollback:**
  - If the new version added **no** migration (check `db/migrations/`), roll back the image: `kubectl -n incidentd rollout undo deploy/incidentd`.
  - If it did add one, restore from backup or roll forward with a fix. Older binaries do not know newer columns.
- **Config change:** edit the ConfigMap, then run `kubectl -n incidentd rollout restart deploy/incidentd`. The severity policy and catalog load at startup.
- **Catalog change:** catalog files are baked into the image. Changes go through code review (CODEOWNERS) and a new image.

## 19. Enabling remediation (later phase)

Do this **only** after all of the following:

1. The triage evaluation gate passes on at least 20 incidents, with at least 10 historical ones (see [triage-evaluation.md](triage-evaluation.md)).
2. Security review signs off.
3. `deploy/k8s/rbac-remediation.yaml` is applied **per allowed namespace** (patch deployments, update deployments/scale).

Then turn on both keys:

1. **Env key:** set `REMEDIATION_ENABLED=true` in the Deployment and roll it out.
2. **Runtime key:**

   ```bash
   curl -X POST -H "Authorization: Bearer $ADMIN" "localhost:8080/admin/remediation?enabled=true"
   ```

`incidentd_remediation_executor_enabled` becomes `1` and the `IncidentdExecutorEnabled` info alert fires.
To kill execution instantly, set the runtime key to `false`.

## 20. Troubleshooting the install

| Symptom | Likely cause | Fix |
|---|---|---|
| Pod crash-loops with `invalid configuration` | Missing secret or required field | The message lists **every** problem; fix them and restart. |
| `create schema … (pre-create it if the role lacks CREATE)` | App role cannot create the schema | Pre-create the schema as DBA (section 4). |
| `/incident` shows "dispatch_failed" in Slack | Slack can't reach the ingress, or TLS is invalid | Check the ingress host, certificate and the Request URL in the Slack app. |
| Slack returns 401 in logs (`slack command rejected`) | Wrong signing secret, or clock skew > 5 min | Fix `SLACK_SIGNING_SECRET`; sync node time (NTP). |
| War room not created | Missing `channels:manage` / `groups:write`, or the app wasn't reinstalled after scope changes | Reinstall the Slack app. |
| On-call not invited | PagerDuty email doesn't match a Slack email | Align emails; identity is email-based. |
| Breaches never auto-declare | No `MULTIPLEXER_TOKEN` (tier unknown), or tenant not assigned to the view | Create the token; assign the view to a tenant in Multiplexer. |
| Triage says "AI triage is not configured" | `llm.enabled: false` | Enable it and set `ANTHROPIC_API_KEY`. |
| Triage tool errors for Kubernetes | Namespace not in allow-list, or RBAC missing | Add it to `kubernetes.clusters[].namespaces` and bind the read role. |
