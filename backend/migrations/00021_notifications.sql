-- Notifications (docs/design/05-delivery-and-approvals.md#notifications, P4-07).
--
-- One row per event, recipient and channel: a job fans each notified event out to its recipients,
-- and a job per row sends it. The key makes each sent once, however often the fan-out runs. A
-- recipient is user:<name>, a person of the directory, whose address is read when the message is
-- sent, or email:<address>, an address of notifications.alerts. Rows Quai could not send are kept
-- with why: no address, the request no longer waiting, the attempts exhausted.
--
-- Personal data: the recipient's name or address, as the events and reviews hold names.

-- +goose Up
CREATE TABLE notifications (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id     text NOT NULL DEFAULT 'default',
    event_id   bigint NOT NULL REFERENCES events (id) ON DELETE CASCADE,
    recipient  text NOT NULL,
    channel    text NOT NULL,
    -- What the message says: submitted, approval_needed, approved, rejected, stale, merged,
    -- applied, failed, expiring_soon or alert.
    kind       text NOT NULL,
    -- pending until sent; skipped (nothing to send, last_error says why); failed (no attempt left).
    state      text NOT NULL DEFAULT 'pending',
    attempts   integer NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    sent_at    timestamptz,
    UNIQUE (event_id, recipient, channel)
);

-- +goose Down
DROP TABLE notifications;
