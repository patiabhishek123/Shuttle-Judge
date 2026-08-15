# SCHEMA.md — Shuttle Court (Postgres)

Design notes: everything keyed off `case_id`. No user-account concept — a "party" is just a channel identity linked to a case. Kept deliberately small (7 tables) for a 24h build; every table is something TASKS.md and TEST.md directly exercise.

```sql
-- Enable UUIDs
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- =========================================================
-- cases: one row per dispute
-- =========================================================
CREATE TABLE cases (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    short_id          VARCHAR(6) UNIQUE NOT NULL,        -- human-friendly, shown in email subject etc.
    join_code         VARCHAR(6) UNIQUE NOT NULL,
    join_code_used_at TIMESTAMPTZ,                        -- null until Party B joins; enforces single-use
    topic_summary     TEXT,                                -- one-line neutral summary, safe to show to B before intake
    status            VARCHAR(20) NOT NULL DEFAULT 'INTAKE'
                        CHECK (status IN (
                          'INTAKE','AWAITING_JOIN','INTAKE_B','CROSS_CHECK',
                          'PROPOSE','AWAITING_CONSENT','RESOLVED','STALLED'
                        )),
    cross_check_rounds SMALLINT NOT NULL DEFAULT 0,
    cross_check_rounds_a SMALLINT NOT NULL DEFAULT 0,
    cross_check_rounds_b SMALLINT NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at       TIMESTAMPTZ,
    delivery_issue    TEXT
);

-- =========================================================
-- parties: one row per side of a case (exactly 2 per case in v1)
-- =========================================================
CREATE TABLE parties (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    case_id     UUID NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
    role        VARCHAR(1) NOT NULL CHECK (role IN ('A','B')),
    display_ref VARCHAR(120),                              -- e.g. "telegram user 8213..." — never a real name unless party gave one
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (case_id, role)
);

-- =========================================================
-- conversation_links: maps a Caspian conversation to (case, party)
-- This is the table that makes "any channel, either side" work.
-- =========================================================
CREATE TABLE conversation_links (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id VARCHAR(255) NOT NULL,   -- Caspian's conversation id
    channel         VARCHAR(20) NOT NULL CHECK (channel IN ('telegram','slack','email')),
    case_id         UUID NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
    party_id        UUID NOT NULL REFERENCES parties(id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, channel)
);
CREATE INDEX idx_conversation_links_lookup ON conversation_links (conversation_id, channel);

-- =========================================================
-- claims: structured facts extracted from a party's free text
-- Never displayed raw to the other party — see AGENTS.md §2
-- =========================================================
CREATE TABLE claims (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    case_id      UUID NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
    party_id     UUID NOT NULL REFERENCES parties(id) ON DELETE CASCADE,
    claim_type   VARCHAR(30) NOT NULL CHECK (claim_type IN
                   ('amount','date','who_did_what','desired_outcome','other')),
    value_text   TEXT NOT NULL,          -- normalized value, e.g. "340.00", "2026-03-14"
    confidence   VARCHAR(10) NOT NULL DEFAULT 'stated' CHECK (confidence IN ('stated','vague','confirmed')),
    source_message_id UUID REFERENCES messages_log(id),  -- audit trail back to raw text (internal only, never surfaced)
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_claims_case_type ON claims (case_id, claim_type);

-- =========================================================
-- proposals: generated resolution text + version, shared verbatim to both parties
-- =========================================================
CREATE TABLE proposals (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    case_id          UUID NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
    version          SMALLINT NOT NULL,
    proposal_text    TEXT NOT NULL,
    generated_from   JSONB NOT NULL,        -- snapshot of the claims used, for judge-facing audit / debugging
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (case_id, version)
);

-- =========================================================
-- consents: each party's response to a specific proposal version
-- =========================================================
CREATE TABLE consents (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    proposal_id      UUID NOT NULL REFERENCES proposals(id) ON DELETE CASCADE,
    party_id         UUID NOT NULL REFERENCES parties(id) ON DELETE CASCADE,
    decision         VARCHAR(10) NOT NULL CHECK (decision IN ('yes','no')),
    comment          TEXT,                 -- only ever shown back to the SAME party, never the counterpart
    objection_claim_ids JSONB NOT NULL DEFAULT '[]',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (proposal_id, party_id)
);

-- =========================================================
-- messages_log: raw inbound/outbound audit trail (for judges / debugging only)
-- =========================================================
CREATE TABLE messages_log (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    case_id         UUID REFERENCES cases(id) ON DELETE CASCADE,
    party_id        UUID REFERENCES parties(id) ON DELETE CASCADE,
    direction       VARCHAR(3) NOT NULL CHECK (direction IN ('in','out')),
    channel         VARCHAR(20) NOT NULL,
    raw_text        TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_messages_log_case ON messages_log (case_id, created_at);
```

## Notes
- `outbound_messages` in `schema.sql` is the transactional delivery outbox. It commits proactive delivery intent with case state and retries delivery asynchronously.
- `claims.source_message_id` references `messages_log` — declare `messages_log` before `claims` in real migration order, or defer the FK (shown out of creation-order above for readability; migration file should create `messages_log` first).
- No `users` table on purpose — identity is entirely `(conversation_id, channel)` scoped through `conversation_links`. This matches AGENTS.md's non-goal of persistent accounts.
- `parties.display_ref` is intentionally opaque — do not store phone numbers/emails in plaintext beyond what's operationally needed to reply via Caspian (Caspian itself owns the actual channel address).
