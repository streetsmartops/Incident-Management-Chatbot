# Operations runbook

How to run `incidentd` day to day. The alert rules in `deploy/monitoring/` link to the sections below.

- [Health and logs](#health-and-logs)
- Alert runbooks:
  - [incidentd down](#incidentd-down)
  - [SLO polling failing](#slo-polling-failing)
  - [Side-effect failures](#side-effect-failures)
  - [Signature failures](#signature-failures)
  - [Triage errors](#triage-errors)
  - [Slow acks](#slow-acks)
  - [Kill switch](#kill-switch)
- Routine tasks:
  - [Verify an audit trail](#verify-an-audit-trail)
  - [Rotate credentials](#rotate-credentials)
  - [Change the severity policy](#change-the-severity-policy)
  - [Change the action catalog](#change-the-action-catalog)
  - [Re-arm auto-declare for an SLO](#re-arm-auto-declare-for-an-slo)
  - [Backups and data retention](#backups-and-data-retention)

## Health and logs

| Check | Command |
|---|---|
| Liveness / readiness | `GET /healthz`, `GET /readyz` (readiness pings the database) |
| Logs | `kubectl -n incidentd logs deploy/incidentd -f` |
| Metrics | `kubectl -n incidentd port-forward svc/incidentd-metrics 9090` then `curl localhost:9090/metrics` |
| Config validation | `incidentd config-check -config /etc/incidentd/config.yaml` |

Logs are JSON. Useful keys:
- `incident_id` and `correlation_id`: per incident.
- `request_id`: per HTTP request; also returned in the `X-Request-Id` header.
- `kind` on `msg=timeline`: one line per timeline event.
- `step` on `msg="side effect failed"`.

Secrets are never logged.

## incidentd down

**Impact:**
- Chat commands fail; Slack shows "dispatch_failed".
- PagerDuty webhooks are retried by PagerDuty.
- SLO breaches are **not** declared.

**Responders can always fall back to paging directly in PagerDuty.**

1. Check the pods:

   ```bash
   kubectl -n incidentd get pods
   ```

   ```bash
   kubectl -n incidentd describe pod -l app=incidentd
   ```

2. Crash-looping with `invalid configuration`: the log lists every missing or invalid setting. Fix the Secret or ConfigMap and roll out again.
3. `database:` errors: check Postgres reachability and credentials, and that the schema exists.
4. Not ready (`/readyz` 503): the database is unreachable from the pod. Check NetworkPolicy and egress.
5. After recovery, check for missed SLO breaches in Multiplexer. The poller resumes on start and declares anything still breached that has no open incident (after two evaluations).

## SLO polling failing

**Impact:** breaches in Multiplexer will not be proposed or auto-declared.

1. Look for `multiplexer poll failed` in the logs; the error names the HTTP status.
2. **401/403:** `MULTIPLEXER_TOKEN` is expired, revoked or missing the `views:read` scope (after PR #5, `/api/slo-guardrails` requires auth). Create a new token and roll out.
3. **Timeouts or connection errors:** network path to Multiplexer.
4. **503 "Database schema not ready":** Multiplexer itself is unhealthy. Escalate to the Multiplexer owner.

## Side-effect failures

Declaring an incident never fails because of a side effect. Each failure is written to the
incident timeline as an `error` event and counted in `incidentd_side_effect_failures_total{step}`.

| `step` | Meaning | Check |
|---|---|---|
| `page` | PagerDuty incident creation failed | Token validity, `fromEmail` is a real PagerDuty user, the service ID exists |
| `itsm_ticket`, `itsm_comment` | Jira call failed | Token, project permissions, issue type name |
| `war_room` | Channel creation failed | Slack scopes (`channels:manage`, `groups:write`); reinstall the app |
| `invite_ic`, `invite_oncall`, `lookup_user` | Invite failed | Slack email doesn't match the PagerDuty email |
| `set_topic`, `post*` | Slack posting failed | Bot removed from channel, or rate limited |
| `page_resolve`, `page_note` | PagerDuty write failed | As for `page` |

Fix the cause. For the affected incident, do the missed step by hand (for example, page in PagerDuty)
and record it with `/incident update "Paged manually: …"`.

## Signature failures

Many 401s on `/slack/*`, `/teams/messages` or `/webhooks/pagerduty`.

- **Right after a secret rotation:** the Secret or deployment is out of date. Roll out again.
- **Slack only:** clock skew over 5 minutes on nodes (requests are rejected as replays). Check NTP.
- **Otherwise:** someone may be sending spoofed requests. The bot rejects them; nothing reaches the core. Review the source IPs in ingress logs and consider a WAF rule.

## Triage errors

1. Look for the incident's `triage failed:` chat message, or `msg="side effect failed" step=triage`.
2. `anthropic API error (HTTP 401)`: invalid `ANTHROPIC_API_KEY`. `HTTP 429`: rate or spend limit. `HTTP 5xx`: transient; retry `/incident triage`.
3. `model declined this request (refusal)`: rare. Triage that incident manually.
4. `model did not submit a triage result` / `invalid triage result twice`: the model ran out of budget or produced a malformed result. Retry with `--focus "…"` to narrow it.
5. A tool error inside the result (for example Kubernetes forbidden) is not a triage failure. Fix the RBAC or the allow-list.

Triage is advisory. Responders can always continue without it.

## Slow acks

Slack needs an HTTP 200 within 3 seconds. `incidentd` acks immediately and does the work
asynchronously, so slow acks mean the pod is starved or the database is slow, not that the work is slow.

1. Check CPU throttling and memory on the pods; raise requests and limits.
2. Check Postgres latency. Request handling does no database work before the ack, but the pool shares the node.
3. Check ingress latency.

## Kill switch

Remediation execution needs **both**:

- env `REMEDIATION_ENABLED=true` on the Deployment, and
- runtime flag `remediation_enabled` = true in the database.

| Task | How |
|---|---|
| See current state | Gauge `incidentd_remediation_executor_enabled`, or the response of the admin call below |
| **Stop all execution now** | `curl -X POST -H "Authorization: Bearer $ADMIN" "<internal>/admin/remediation?enabled=false"` |
| Turn the env key off | Set `REMEDIATION_ENABLED=false` and roll out |
| Re-enable | Only per [deployment.md §19](deployment.md#19-enabling-remediation-later-phase) |

While either key is off:
- Approved actions are recorded as `dry_run_only` with the diff shown.
- Responders perform the change manually.

The `IncidentdExecutorEnabled` info alert fires whenever execution is on, so the state is never forgotten.

## Verify an audit trail

Each incident's timeline is a SHA-256 hash chain. To check it:

```bash
incidentd verify-timeline -config /etc/incidentd/config.yaml INC-2026-0042
```

Or call `GET /admin/timeline/verify?incident=INC-2026-0042` with the admin token. The response reports
`intact`, the number of events, and the first bad sequence number if the chain was altered.
The database also blocks `UPDATE`/`DELETE`/`TRUNCATE` on `timeline_events`.

## Rotate credentials

1. Create the new credential in the vendor tool. Keep the old one valid.
2. Update the value in your secret manager or the `incidentd-secrets` Secret.
3. `kubectl -n incidentd rollout restart deploy/incidentd`
4. Confirm there are no `side effect failed` or 401 entries, then revoke the old credential.

| Secret | Rotation notes |
|---|---|
| `SLACK_SIGNING_SECRET` | Slack can regenerate it. Requests fail until rollout, so do it in a quiet window. |
| `PAGERDUTY_WEBHOOK_SECRET` | Create a new webhook subscription, switch, then delete the old one. |
| `MULTIPLEXER_TOKEN` | Expires (default 90 days). Rotate before expiry. |
| `INCIDENTD_ADMIN_TOKEN` | Any random 32+ byte value. |

## Change the severity policy

1. Copy `config/severity-policy.example.yaml`, edit it, and review it with SRE leads (CODEOWNERS).
2. Mount it and set `severityPolicyFile`. Validate with `config-check`.
3. Roll out. The startup log shows `severity policy loaded hash=…`, and every declaration records that hash in its timeline.

## Change the action catalog

Catalog entries live in `remediation/catalog/*.yaml` and are baked into the image.

1. Open a PR. CODEOWNERS requires SRE lead review.
2. Each entry needs `id`, `provider`, `verb`, `risk`, a parameter schema, and `allowedScopes`.
3. `go test ./remediation/` validates the catalog. Build and roll out a new image.

## Re-arm auto-declare for an SLO

After an incident is resolved, the same SLO breach is not re-declared until the SLO reports `ok`
again, because 30-day windows stay `breached` long after mitigation. If a genuinely new problem appears while
the SLO is still breached, declare it manually with `/incident declare …`. To re-arm automatic
declaration for that guardrail without waiting for recovery, clear the flag:

```sql
UPDATE incident_bot.flags SET value = false WHERE name = 'suppress:multiplexer:guardrail:<ID>';
```

## Backups and data retention

- Back up the Postgres database (all tables in the dedicated schema).
- The timeline is append-only by design. To purge an incident (for example, legal hold expiry), restore a
  filtered copy or drop and recreate the schema from a backup. Document the reason in your change record.
- Eval data from `incidentd eval import-pagerduty` is written to `test/fixtures/historical/` on the
  operator's machine. Keep it out of git if it contains customer data.
