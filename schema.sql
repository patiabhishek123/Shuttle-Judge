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
    cross_check_rounds SMALLINT NOT NULL DEFAULT 0,        -- aggregate count retained for reporting
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

-- Transactional outbox. Mediation state and intended proactive deliveries are
-- committed atomically; a background worker performs network I/O afterwards.
CREATE TABLE outbound_messages (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    case_id         UUID NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
    party_id        UUID NOT NULL REFERENCES parties(id) ON DELETE CASCADE,
    conversation_id VARCHAR(255) NOT NULL,
    channel         VARCHAR(20) NOT NULL,
    text            TEXT NOT NULL,
    status          VARCHAR(12) NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending','processing','sent','failed')),
    attempts        SMALLINT NOT NULL DEFAULT 0,
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at         TIMESTAMPTZ
);
CREATE INDEX idx_outbound_messages_pending
    ON outbound_messages (status, created_at);

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
