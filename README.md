# ⚖️ Shuttle Court

### One mediator. Two private conversations. A resolution both sides choose.

> Built for the **Caspian Hackathon** — an experiment in using one cross-channel agent identity for something that cannot be reduced to “a chatbot on three apps.”

Shuttle Court is a private, two-party mediation system powered by Caspian. One person can open a dispute on Telegram, the other can join from Slack, and the final resolution can be logged through Email. Each person speaks privately with the same neutral mediator, **Docket**, without receiving the other party’s raw messages.

Docket extracts structured claims, checks contradictions, proposes a neutral compromise, and closes the case only after both parties independently accept the same proposal version.

## The hackathon idea

Most omnichannel agents use several channels as interchangeable chat windows. Shuttle Court makes channel separation the product mechanic:

```text
Party A · Telegram ──┐                         ┌── Private intake A
                     │                         │
                     ├── Caspian ── Docket ───┼── Structured cross-check
                     │                         │
Party B · Slack ─────┘                         └── Private intake B
                                  │
                                  └── Email resolution log
```

Two people can participate from different apps while Caspian maintains one agent identity and the Go engine preserves case continuity, privacy, and consent.

## The four-minute demo

1. A judge opens a roommate electricity-bill dispute on Telegram.
2. Docket creates a case and returns a six-character join code.
3. A second judge sends that code to Docket from Slack.
4. Docket interviews both judges privately, one question at a time.
5. One judge reports `$340`; the other reports `$300`.
6. The deterministic cross-check detects the contradiction and asks for clarification without exposing either account.
7. Docket generates one neutral proposal from structured claims and sends identical text to both sides.
8. The case becomes `RESOLVED` only after two independent `YES` responses to the same proposal version.
9. A resolution record can be delivered through Email for the final cross-channel moment.

## The hard privacy rule

> Party A’s raw message is never revealed to Party B, and Party B’s raw message is never revealed to Party A.

This is an architectural boundary, not merely a prompt instruction:

- Raw messages are converted into typed claims such as `amount`, `date`, `who_did_what`, and `desired_outcome`.
- Cross-checking operates on structured claims.
- Proposals and revisions are generated from claim snapshots, never from counterpart message text.
- Rejection comments are privately recorded and linked to their extracted claims.
- “What did they say?” receives a refusal plus only the neutral dispute topic.
- Consent and amount/date contradiction decisions are deterministic Go logic rather than LLM decisions.

## How it works

```text
Telegram / Slack / Email
          │
          ▼
  Caspian channel adapter
  JavaScript · transport only
          │  POST /message
          ▼
  Go mediation engine
  deterministic state machine
          │
          ├── OpenRouter / Claude
          │   claim extraction + neutral generation
          │
          ▼
      PostgreSQL
  cases · claims · proposals
  consents · audit · outbox
```

The JavaScript adapter owns Caspian connectivity and contains no mediation logic. The Go service owns identity resolution, transitions, contradiction checks, proposal versions, consent, and persistence. PostgreSQL also acts as a transactional outbox so proactive messages are committed with case state before network delivery begins.

### Case lifecycle

```text
INTAKE → AWAITING_JOIN → INTAKE_B → CROSS_CHECK
                                      │
                                      ▼
PROPOSE → AWAITING_CONSENT ── both YES ──→ RESOLVED
    ▲              │
    └── concern ───┘

Any bounded clarification failure or inactivity timeout → STALLED
```

## Built with

- **Caspian SDK** — one agent across Email, Telegram, and Slack
- **Go** — deterministic mediation engine and HTTP API
- **PostgreSQL 16** — state, claims, proposal versions, consent, and audit trail
- **OpenRouter + Claude** — structured claim extraction and neutral language generation
- **Docker Compose** — local PostgreSQL development environment
- **Node.js** — deliberately thin Caspian channel adapter

## Quick start

### 1. Prepare configuration

```bash
cp .env.example .env
```

Fill in the required Caspian and OpenRouter credentials. For the included local database container, use:

```env
DATABASE_URL=postgres://tracker_admin:local_dev_password@localhost:5433/shuttle_court?sslmode=disable
```

Use different random values for `ENGINE_TOKEN` and `ADAPTER_TOKEN`. Keep `.env` private.

### 2. Start PostgreSQL

```bash
docker compose up -d postgres
docker compose ps
```

The container binds only to `127.0.0.1:5433`, persists data in a named volume, and initializes the schema automatically.

Verify it:

```bash
docker compose exec postgres \
  psql -U tracker_admin -d shuttle_court -c '\dt'
```

### 3. Start the mediation engine

```bash
go run .
```

In another terminal:

```bash
curl http://localhost:8080/health
```

Expected response: `OK`.

### 4. Start the Caspian adapter

```bash
cd channel-adapter
npm install
node index.js
```

Email connects by default. Telegram and Slack are enabled when their corresponding values are present in `.env`.

## Verify the build

```bash
GOCACHE=/tmp/shuttle-court-go-cache go test -race ./...
GOCACHE=/tmp/shuttle-court-go-cache go vet ./...

cd channel-adapter
npm test
```

With PostgreSQL, the Go engine, the adapter, OpenRouter, and Caspian running, execute the full scripted flow:

```bash
node channel-adapter/test-mediation-flow.js
```

The script opens a case, joins Party B, introduces a contradiction, reaches a proposal, records two consents, and fails with a nonzero exit code unless it observes the final resolved confirmation.

## Prove it is not mocked

During the demo, inspect the latest case directly:

```bash
docker compose exec postgres psql -U tracker_admin -d shuttle_court -c "
SELECT short_id, status, topic_summary,
       cross_check_rounds, resolved_at, delivery_issue
FROM cases
ORDER BY created_at DESC
LIMIT 5;"
```

Then show independent consent to the same proposal:

```bash
docker compose exec postgres psql -U tracker_admin -d shuttle_court -c "
SELECT ca.short_id, pr.version, pa.role, co.decision
FROM consents co
JOIN proposals pr ON pr.id = co.proposal_id
JOIN cases ca ON ca.id = pr.case_id
JOIN parties pa ON pa.id = co.party_id
ORDER BY co.created_at;"
```

The database exposes the full state transition story while raw messages remain internal and are never relayed between parties.

## Safety and scope

Docket mediates ordinary interpersonal and financial disagreements. It refuses cases involving violence, self-harm, threats, or illegal activity, and it will not relay messages that are only insults. Shuttle Court provides structured mediation—not legal advice, arbitration, or an enforceable judgment.

Version one is intentionally bilateral: two parties, one case, independently recorded consent.

## Repository guide

| Path | Purpose |
|---|---|
| `main.go` | Go mediation engine, state machine, LLM client, and delivery workers |
| `channel-adapter/` | Caspian channel connections and transport adapter |
| `schema.sql` | Canonical PostgreSQL schema |
| `AGENTS.md` | Docket’s privacy and behavior contract |
| `DESIGN.md` | Architecture decisions and request lifecycle |
| `PRD.md` | Hackathon product story and success criteria |
| `main_test.go` | Deterministic behavior regression tests |
| `compose.yaml` | Local-only PostgreSQL service |

## Why Shuttle Court?

Because a shared agent does not have to mean a shared conversation.

Caspian gives Docket one identity everywhere. Shuttle Court turns that identity into a neutral space between channels—private enough for disagreement, structured enough for accountability, and human enough to end with a choice from both sides.
