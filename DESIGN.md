# DESIGN.md — Shuttle Court

## 1. Architecture overview

```
 Telegram   Slack   Email
     \        |       /
      \       |      /
     caspian-sdk (TypeScript) — "channel-adapter" service
     - one on_message handler
     - no mediation logic here
              |
              | HTTP POST /message  { conversation_id, channel, text, sender_ref }
              v
     Go mediation engine (net/http or chi/gin)
     - resolves conversation_id -> case_id + party_role  (conversation_links table)
     - deterministic state machine (case.status)
     - calls Claude API for: claim extraction, proposal/question generation
     - reads/writes Postgres
              |
              v
          Postgres
     cases, parties, conversation_links, claims, proposals, consents, messages_log
```

Why split TS/Go instead of doing it all in one Node process: the SDK is Node-only, and the mediation engine is the part worth being careful, typed, and testable under time pressure — keeping it a separate Go service means the channel layer can be rewritten or reconnected (e.g. swap Slack for WhatsApp later) without touching mediation logic, and it forces the "no LLM in the transport layer" rule in AGENTS.md §5 to be structurally true, not just a convention.

## 2. Request lifecycle (single inbound message)

1. Caspian SDK fires `on_message(message)`. `message.text`, `message.conversation_id` (per Caspian's conversation-continuity model), and channel type are available.
2. TS handler POSTs `{ conversation_id, channel, text }` to the Go engine's `/message` endpoint. No business logic — a pure forward.
3. Go engine:
   a. Looks up `conversation_links` by `(conversation_id, channel)`. If none exists → treat as a fresh entry point (case_open / case_join / other, see AGENTS.md §4).
   b. If linked to an existing case, loads `case.status`, dispatches to the phase handler for that status.
   c. Phase handler may call Claude (extraction or generation) and/or mutate DB state (insert claim, insert proposal, insert consent, transition `case.status`).
   d. Returns `{ reply_text }` to TS.
4. TS calls `message.reply(reply_text)` — Caspian routes it back to the correct channel/thread automatically.

This is intentionally a synchronous request/response loop per message — no queue needed for a two-party, low-throughput demo. (Kafka is listed as an optional stretch item in TASKS.md if there's spare time and the team wants to show event-log thinking, but it is **not** on the critical path — do not build it first.)

## 3. Join-code flow (the mechanic that makes the demo work)
Caspian's outbound-proactive-messaging capability per channel is not something to depend on for a 24h build. So Party B always **initiates contact** with the agent themselves:

1. Party A opens a case → engine generates a short `join_code` (6 chars, unambiguous alphabet) → returned to A.
2. Party A shares the code with Party B out of band (this is realistic — that's how you'd actually invite someone to a dispute-resolution flow).
3. Party B messages the agent, from **any** connected channel, with the code (agent recognizes a bare 6-char code as `case_join` intent per AGENTS.md §4).
4. Engine inserts a `conversation_links` row binding B's `(conversation_id, channel)` to `case_id` with `party_role = 'B'`, transitions `case.status` from `AWAITING_JOIN` to `INTAKE_B`.

This avoids needing platform-specific proactive-send permissions entirely and is the most demo-robust option under time pressure.

## 4. Contradiction detection (CROSS_CHECK phase)
Kept deliberately narrow in scope for reliability over sophistication:
- Only compares `claims` rows that share a `claim_type` between the two parties (`amount`, `date`, `who_did_what`, `desired_outcome`).
- `amount`/`date` types: numeric/date diff beyond a small tolerance = contradiction.
- `who_did_what` type: cheap heuristic first (do the extracted subject/object pairs disagree), Claude call only if heuristic is ambiguous — keeps latency and API calls low for a live demo.
- On contradiction: engine does **not** tell either party what the other claimed. It re-asks the party whose claim was less specific/verifiable ("Can you confirm the exact date the bill was due?") — see AGENTS.md §3 step 4.

## 5. Proposal generation
Input to Claude: the reconciled `claims` rows only (structured JSON), never raw message text. Output: a short neutral proposal (1–3 sentences) plus a `proposal_version` int. Both parties get identical text. This is the point where "never leak raw text" is easiest to violate accidentally, so it's enforced by *not passing raw text into that prompt call at all* — not by asking the model nicely.

## 6. Channel-specific rendering
- **Telegram / Slack**: plain text, short. Slack may use simple bold for the proposal line via Caspian's rich-message support if trivial; not required for MVP.
- **Email**: needs a subject line convention: `Shuttle Court — Case {short_id}`. Slightly more formal opening line. Same content otherwise.
- The TS layer does **not** do channel-specific formatting logic beyond what Caspian's per-channel rendering already gives for free — keep this thin.

## 7. Failure modes considered
- Party B never joins → case sits `AWAITING_JOIN` indefinitely; `status_query` from A reports this honestly.
- Both parties reject the same proposal twice → after 2 revision rounds, case moves to `STALLED` rather than looping forever (protects the live demo from an infinite loop if a judge deliberately stalls it).
- Duplicate join attempts (someone else guesses/reuses a code) → join codes are single-use, checked against `cases.join_code_used_at`.

## 8. What's explicitly out of scope for DESIGN
Auth, rate limiting, multi-tenant isolation beyond per-case row scoping, retries/idempotency on the Caspian webhook delivery (assume happy path for demo), horizontal scaling. All acceptable to skip for a 24h single-demo build; noted so nobody spends time on them by accident.
