# Diagrams

Mermaid sources are in [`diagrams/`](diagrams/), rendered to SVG (shown here) and 2× PNG.
Re-render with `docs/diagrams/render.sh`.

## Component architecture

One Go service. Chat and ingest adapters feed the incident core, which drives the triage,
remediation and postmortem engines. All vendor access goes through `providers/` (ports and adapters).

![Component architecture](diagrams/architecture.svg)

## Deployment topology

Public ingress exposes only the chat and webhook paths. Metrics stay cluster-internal. Read-only
RBAC is bound per allowed namespace; the remediation Role ships but is not applied in v1.

![Deployment topology](diagrams/deployment.svg)

## Data flow: signals → incidents

How a Multiplexer SLO status or a PagerDuty webhook becomes an attached signal, a proposal or an
auto-declared incident, including de-duplication and post-resolve suppression.

![Signal data flow](diagrams/dataflow-signals.svg)

## Data flow: AI triage and trust boundaries

What the model can reach (read-only tools), what it sees (redacted, wrapped data) and what is
enforced in code after it answers.

![Triage data flow](diagrams/dataflow-triage.svg)

## Sequence: a Slack command

![Slack command sequence](diagrams/sequence-command.svg)

## Sequence: human-in-the-loop remediation

![Remediation sequence](diagrams/sequence-remediation.svg)

## Incident lifecycle

![Lifecycle](diagrams/lifecycle.svg)

## Data model (Postgres)

![Data model](diagrams/data-model.svg)
