-- Approval cards in chat (docs/design/11-integrations.md#microsoft-teams, P4-09).
--
-- A personal approval card Quai sent in chat (Microsoft Teams), where it is, and the change set it
-- showed, so that Quai updates it once the request no longer waits for that person: decided,
-- changed, approved by them, or no longer theirs to approve. closed_at is set when the card was
-- updated to say so; a card is updated once.
--
-- Personal data: the recipient's name, and the chat conversation Quai has with them.

-- +goose Up
CREATE TABLE chat_cards (
    notification_id bigint PRIMARY KEY REFERENCES notifications (id) ON DELETE CASCADE,
    org_id          text NOT NULL DEFAULT 'default',
    request_id      uuid NOT NULL REFERENCES requests (id) ON DELETE CASCADE,
    channel         text NOT NULL,
    recipient       text NOT NULL,
    -- Where the card is: the conversation and the message (activity) in it.
    conversation    text NOT NULL,
    message         text NOT NULL,
    -- The hash of the change set the card showed, and its Approve and Deny links are bound to.
    change_set      text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    closed_at       timestamptz
);

CREATE INDEX chat_cards_open ON chat_cards (request_id) WHERE closed_at IS NULL;

-- +goose Down
DROP TABLE chat_cards;
