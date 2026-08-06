# PRD.md — Shuttle Court

## 1. One-line pitch
An AI mediator with one identity across Telegram, Slack, and Email that mediates a real dispute between two people **who never see each other's messages** — gathering each side privately, cross-checking their accounts against each other, and only closing the case once both consent independently, on whichever channel they each prefer.

## 2. Why this is the entry
Hackathon judging criterion: *most creative use case that actually works, live.* Caspian's differentiator is one agent identity reachable across channels with persistent per-thread conversations. Almost every team will use that as "chatbot, but omnichannel." Shuttle Court instead makes the multi-channel, mutually-invisible property of the identity **the entire mechanic** — it is structurally impossible to build this on a single-channel bot. It is also trivially live-demoable: two judges, two phones, two different apps, one visible resolution.

## 3. Target demo scenario
Two judges play a dispute (e.g. a roommate electricity-bill argument). Judge A opens a case on Telegram. Judge B joins on Slack using a join code. The agent privately interviews both, in parallel, without either seeing the other's answers. It detects a factual contradiction between the two accounts, asks a clarifying follow-up to whichever party caused it, proposes a resolution, and requires separate consent from both before declaring the case resolved. A third channel (Email) is used to show the case summary going to a neutral "case log" recipient.

## 4. Core user stories
1. **As Party A**, I can message the agent on any supported channel to open a new case describing a dispute.
2. **As Party A**, I receive a short join code to share with Party B out-of-band (text, verbally, etc.).
3. **As Party B**, I can message the agent on *any* supported channel (not necessarily A's channel) with the join code to attach myself to the case.
4. **As either party**, I am privately interviewed by the agent about my account of the dispute — I never see the other party's raw messages.
5. **As the agent**, when the two accounts conflict on a checkable fact (amount, date, who-did-what), I ask a targeted follow-up to resolve the contradiction before proposing anything.
6. **As either party**, I receive a proposed resolution phrased neutrally (not "attributed" to either side) and must explicitly consent (`yes` / `no` / counter-comment).
7. **As the agent**, I only mark a case RESOLVED when both parties have consented to the same proposal version. If either rejects, I revise and re-send to both.
8. **As either party**, I can ask the agent for case status at any time and get an honest answer ("waiting on the other party," "waiting on you," etc.) without leaking content.

## 5. Explicit non-goals (cut for time)
- No legally binding outcomes framing — this is presented as "structured mediation," not legal arbitration.
- No group disputes (>2 parties) — v1 is strictly bilateral.
- No voice/call channels, no WhatsApp/X/iMessage (paid/unavailable in time budget).
- No user auth beyond channel identity + join code. No persistent user accounts across cases.
- No editing a resolution after both parties consent — a resolved case is immutable (can only be reopened as a new case).
- No fairness/bias scoring, no analytics dashboard — a working CLI/DB query is enough to prove state for the demo.

## 6. Success criteria for the demo
- [ ] Case can be created from **any** of the 3 connected channels.
- [ ] Party B can join from a **different** channel than Party A, using only the join code.
- [ ] Agent never echoes the other party's literal text back verbatim to the counterpart (paraphrase only).
- [ ] At least one contradiction-detection → clarifying-question loop fires correctly in the scripted demo.
- [ ] Resolution requires two independent "yes" replies before the case flips to RESOLVED.
- [ ] Full case history (both sides + proposal + consents) is inspectable in Postgres for judges who ask "how do we know it's not mocked."

## 7. Risks
| Risk | Mitigation |
|---|---|
| Caspian channel setup (Slack app, Telegram bot token) not finished before demo | Do channel connection setup first, hour 1, before any logic |
| LLM produces a "resolution" that leaks one party's private wording to the other | Hard rule in the Go prompt layer: proposal text is regenerated from structured extracted fields, never a direct paraphrase-passthrough of raw message text |
| Timing: solo, 24h | See TASKS.md — cut order is Email → Slack → contradiction-detection sophistication, in that priority, if behind schedule |
