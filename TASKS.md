# TASKS.md — Shuttle Court (solo, ~24h)

Ordered so that at every checkpoint you have something demoable, even if you stop early. **Cut order if behind schedule: Email → Slack-polish → cross-check sophistication → Kafka stretch goal.** Never cut: join-code flow, "no raw text leaks" rule, two-consent gate.

## Hour 0–1: Environment & channel wiring (do this first, it's the riskiest external dependency)
- [ ] Caspian account, API key, `caspian-sdk` installed (TS)
- [ ] Connect Telegram (`connect_telegram()`, BotFather token) — confirm round-trip echo works
- [ ] Connect Slack (`connect_slack()`, one-click app) — confirm round-trip echo works
- [ ] Connect Email (`connect_email()`) — confirm round-trip echo works
- [ ] Postgres running locally (docker), `psql` reachable
- [ ] Go module + Postgres driver (pgx) + basic HTTP server skeleton, health check endpoint

**Checkpoint:** all 3 channels can echo a message back through Caspian. If any channel fails here, cut it now — don't debug a channel mid-build.

## Hour 1–3: Schema + skeleton request loop
- [ ] Run SCHEMA.md migrations
- [ ] Go: `/message` endpoint that just logs to `messages_log` and returns a static reply
- [ ] TS: `on_message` handler POSTs to Go `/message`, replies with whatever Go returns
- [ ] Confirm: message sent on Telegram → shows up in `messages_log` → static reply comes back on Telegram

**Checkpoint:** full transport loop proven end-to-end, before any mediation logic exists.

## Hour 3–6: Case open + join-code flow
- [ ] `case_open` intent detection (simple heuristic: no active case + message not a bare join code + not empty)
- [ ] Create `cases` row, generate `short_id` + `join_code`, create `parties` row (role A), create `conversation_links` row
- [ ] Reply to A with join code + instructions
- [ ] `case_join` intent detection (bare 6-char code matching an unused `join_code`)
- [ ] Create `parties` row (role B), `conversation_links` row, mark `join_code_used_at`, transition case to `INTAKE_B`

**Checkpoint:** open a case on Telegram, join it on Slack, confirm both `conversation_links` rows point at the same `case_id` with different roles.

## Hour 6–10: Intake + claim extraction
- [ ] Claude API call: free text → structured claims (`claim_type`, `value_text`, `confidence`) — write and hand-test the prompt against 5–6 sample dispute descriptions before wiring it in
- [ ] INTAKE phase: up to 3 clarifying questions for Party A, one at a time (AGENTS.md §4)
- [ ] Same for INTAKE_B
- [ ] `topic_summary` generated after A's intake (neutral, safe to show B before B's own intake starts)

**Checkpoint:** run full intake for both sides on two real channels, inspect `claims` table, confirm no cross-contamination.

## Hour 10–14: Cross-check + contradiction loop
- [ ] Implement the `amount`/`date` numeric/date-diff comparison (cheap, deterministic)
- [ ] Implement `cross_check_rounds` increment + bound at 2
- [ ] Targeted follow-up question generation when a contradiction is found
- [ ] Manually construct a demo scenario with a deliberate contradiction (e.g. A says $340, B says $280) and confirm the loop fires exactly once and resolves

**Checkpoint:** this is the single most "wow" moment of the demo — do not skip testing it live across two real channels before moving on.

## Hour 14–17: Proposal + consent
- [ ] Proposal generation from reconciled `claims` only (never raw text — enforce by not passing raw text into this prompt call)
- [ ] Send identical proposal text to both parties
- [ ] Consent capture (`yes`/`no`/comment), `consents` table
- [ ] Two-consent gate → `RESOLVED`; single `no` → revise, increment `proposal.version`, re-send to both (counterpart told only that a concern was raised, not its content)

**Checkpoint:** full case, open → join → intake both sides → contradiction → resolved, run live on Telegram + Slack.

## Hour 17–19: Email as third channel + status query
- [ ] Wire Email the same way (should be near-zero new logic if TS layer stayed thin — this is the test of DESIGN.md's architecture bet)
- [ ] `status_query` intent + honest phase-appropriate reply without leaking content
- [ ] Send resolved-case summary to a neutral email address for "case log" demo moment

## Hour 19–21: Guardrails + refusals
- [ ] Illegal/self-harm/violence topic refusal (AGENTS.md §6)
- [ ] Pure-insult-no-claim refusal
- [ ] STALLED transition after 2 failed revision rounds
- [ ] Duplicate/expired join code handling

## Hour 21–23: Rehearse the live demo, twice, cold
- [ ] Script exact messages for both parties, including the deliberate contradiction
- [ ] Time it — should be under 4–5 minutes end to end
- [ ] Have a **second** pre-seeded case in the DB ready to show `RESOLVED` state instantly if live demo has channel flakiness
- [ ] Prepare the "prove it's not mocked" moment: a `psql` query judges can watch run live against `messages_log`/`claims`/`consents`

## Hour 23–24: Buffer
- [ ] Nothing new. Fix whatever broke in rehearsal only.

## Stretch (only if truly ahead of schedule)
- [ ] Kafka event log for case state transitions (showcases distributed-systems background — genuinely optional, not judged on this criterion)
- [ ] Slack rich-message formatting for the proposal
- [ ] `who_did_what` Claude-assisted contradiction detection (currently heuristic-only in MVP)
