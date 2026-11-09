-- Notification preferences and daily digests (docs/design/05-delivery-and-approvals.md#notifications,
-- P4-08).
--
-- A person's preferences say, per channel and kind of notification, whether it goes at once, in
-- their daily digest or not at all, and when their digest goes: an hour of the day in their time
-- zone. Only what differs from the configuration's defaults (notifications.defaults) is kept: a
-- choice equal to a default follows the default when the configuration changes. Alerts are not
-- among the kinds: they always go at once.
--
-- A notification for the digest waits in the notifications table (state digest) until the digest
-- job gathers it into a digest, one per recipient, channel and period (the local date of the hour
-- it is due): the key makes it sent once however often the job runs, and a notification joins
-- one digest only (digest_id is set once).
--
-- Personal data: the person's name, their time zone, and the recipient of each digest.

-- +goose Up
CREATE TABLE notification_preferences (
    org_id      text NOT NULL DEFAULT 'default',
    name        text NOT NULL,
    -- {"<channel>": {"<kind>": "immediate" | "digest" | "off"}}: the choices that differ from the
    -- defaults.
    modes       jsonb NOT NULL DEFAULT '{}',
    -- When the digest goes: NULL, the defaults'.
    digest_hour smallint CHECK (digest_hour BETWEEN 0 AND 23),
    time_zone   text,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, name),
    -- Erasing someone from the directory erases their preferences.
    FOREIGN KEY (org_id, name) REFERENCES users (org_id, name) ON DELETE CASCADE
);

CREATE TABLE notification_digests (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id     text NOT NULL DEFAULT 'default',
    recipient  text NOT NULL,
    channel    text NOT NULL,
    -- The local date of the hour it was due (due), in the recipient's time zone then.
    period     date NOT NULL,
    due        timestamptz NOT NULL,
    -- pending until sent; skipped (nothing left to tell, last_error says why); failed (no attempt
    -- left).
    state      text NOT NULL DEFAULT 'pending',
    attempts   integer NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    sent_at    timestamptz,
    UNIQUE (recipient, channel, period)
);

-- A notification waits for its digest in state digest; digest_id is the digest that gathered it.
ALTER TABLE notifications
    ADD COLUMN digest_id bigint REFERENCES notification_digests (id) ON DELETE CASCADE;

CREATE INDEX notifications_waiting ON notifications (recipient, channel) WHERE state = 'digest' AND digest_id IS NULL;
CREATE INDEX notifications_digest ON notifications (digest_id) WHERE digest_id IS NOT NULL;

-- +goose Down
DROP INDEX notifications_digest;
DROP INDEX notifications_waiting;
ALTER TABLE notifications DROP COLUMN digest_id;
DROP TABLE notification_digests;
DROP TABLE notification_preferences;
