-- +goose Up
-- M4-12: durable approval message references (spec section 4.1).
--
-- One row per (proposal_id, channel). external_ref is the only durable message
-- reference and is stored as "<channel_id>:<ts>" (Slack channel IDs do not
-- contain ':'). A row is inserted only after a successful chat.postMessage, so
-- there is deliberately no 'posting' state. A crash between the Slack call and
-- the insert is the documented at-least-once duplicate window.
--
-- created_at/updated_at use the database clock (now()) on insert and every
-- state transition; no caller-supplied timestamp is trusted. Transport state
-- is never written to the proposal row.
--
-- Privilege boundary: recon_app is the only runtime role (API interaction
-- reads and outbox posts/updates). There is no DELETE grant because message
-- rows are terminal records. No agent, executor, or audit grants.
CREATE TABLE approval_messages (
    proposal_id  UUID        NOT NULL,
    channel      TEXT        NOT NULL,
    external_ref TEXT        NOT NULL,
    state        TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT approval_messages_pkey PRIMARY KEY (proposal_id, channel),
    CONSTRAINT approval_messages_proposal_id_fkey
        FOREIGN KEY (proposal_id) REFERENCES proposals (id) ON DELETE CASCADE,
    CONSTRAINT approval_messages_channel_check CHECK (channel <> ''),
    CONSTRAINT approval_messages_external_ref_check CHECK (external_ref <> ''),
    CONSTRAINT approval_messages_state_check
        CHECK (state IN ('posted', 'updated', 'cancelled'))
);

REVOKE ALL ON TABLE approval_messages FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE ON approval_messages TO recon_app;

-- +goose Down
REVOKE ALL ON TABLE approval_messages FROM recon_app;
DROP TABLE approval_messages;
