# Developer guide

## Layout

```
cmd/incidentd/     service binary: serve | config-check | verify-timeline | eval
cmd/demo/          local demo console (real engines, fake vendors, scripted LLM)
core/              domain, lifecycle, severity policy, timeline, ports, service, commands, metrics interface
chat/slack, chat/teams        chat adapters (ChatPort) + HTTP handlers
providers/         pagerduty · datadog · jira · kubernetes · multiplexer · anthropic · stubs
ingest/            PagerDuty webhook handler, Multiplexer poller
triage/            agent loop, read-only tool executor, validation, replay executor
remediation/       catalog (+ catalog/*.yaml), approval policy, engine
postmortem/        metrics, BM25 retrieval, drafting
metrics/           Prometheus implementation of core.Metrics
db/                Postgres store + migrations
config/            config loading/validation, example.yaml
eval/              Python: synthetic corpus generator, scorer
test/              fakes, store conformance suite, fixtures
deploy/            Dockerfile-adjacent manifests, dashboards, monitoring, Slack/Teams manifests
docs/              this documentation, diagrams, demo
```

**Rule:** `core` never imports a vendor SDK, `providers/`, or `chat/`. Adapters implement the
interfaces in `core/ports.go`, so swapping an adapter needs no core change. Check it with:

```bash
go list -f '{{join .Imports "\n"}}' ./core/
```

## Build and test

```bash
make build
```

```bash
make test
```

`make test` runs all Go tests plus the Python eval tests.

```bash
TEST_DATABASE_URL=postgres://postgres@localhost:5432/postgres?sslmode=disable make test-db
```

`make test-db` runs the Postgres integration tests: the store conformance suite, multi-replica appends, and the append-only triggers.

```bash
go test -race ./...
```

CI runs this, with a Postgres 16 service.

Testing patterns:
- `test/fakes` provides in-memory ChatPort, PagingProvider, ItsmProvider, SloSource, a Clock, and a ScriptedLLM.
- `test/storetest.Run` is the conformance suite every `core.Store` must pass.
- Adapters are tested against `httptest` servers using response shapes copied from vendor docs and SDK models. Recorded webhook payloads live in `test/fixtures/`.
- Kubernetes tests use `k8s.io/client-go/kubernetes/fake`.

## Run locally

Without credentials, use the demo console:

```bash
go run ./cmd/demo -listen 127.0.0.1:8090
```

Then open <http://127.0.0.1:8090>. See [demo.md](demo.md).

Against a real Slack workspace:

```bash
cp config/example.yaml config/local.yaml
```

Edit it: set `database.driver: memory` and `slack.announceChannelId`.

```bash
export SLACK_BOT_TOKEN=xoxb-… SLACK_SIGNING_SECRET=…
```

```bash
go run ./cmd/incidentd serve -config config/local.yaml
```

Expose `:8080` (for example with ngrok) and point the Slack app's request URLs at it.

## Adding an adapter

1. Implement the port from `core/ports.go` in `providers/<name>/`, using only domain types in signatures.
2. Add a `var _ core.<Port> = (*Client)(nil)` assertion.
3. Test against an `httptest` server with recorded or copied payloads.
4. Add config (`config/config.go` plus validation plus `example.yaml`) and wire it in `cmd/incidentd/main.go`.
5. Document verified vs `UNVERIFIED` details in the package comment and in `docs/architecture.md` Appendix A.

## Adding a catalog action

1. Add `remediation/catalog/<id>.yaml` (see the existing entries). Kubernetes actions must declare `cluster`, `namespace` and `deployment` parameters and `allowedScopes`.
2. If the provider needs a new verb, implement it in `DryRun`, `Execute` and `Verify` with tests (`providers/kubernetes`).
3. Update `deploy/k8s/rbac-remediation.yaml` with exactly the verbs needed.
4. CODEOWNERS requires SRE lead review.

## Adding a metric

1. Add it in `metrics/metrics.go` and to `metrics.Names`.
2. If dashboards use it, update `deploy/dashboards/gen_dashboards.py` (`DD_MAP` too) and regenerate.
3. Run `go test ./metrics/`. It fails if dashboards reference unknown metrics or the k8s annotation mapping is out of date.

## Diagrams

Sources are in `docs/diagrams/*.mmd`. Re-render the SVG and PNG with `docs/diagrams/render.sh`
(mermaid-cli 11, ELK layout).

## Conventions

- Conventional Commits (`feat:`, `fix:`, `docs:` …).
- `gofmt` clean; `go vet` clean.
- Errors shown to users are `*core.UserError`. Everything else is logged and shown as a generic failure.
