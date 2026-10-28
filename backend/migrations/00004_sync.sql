-- Sync (docs/design/01-repository-connections.md#sync): what the health page shows of each
-- repository's webhooks and polling, and the webhook deliveries already taken, so a redelivery
-- is not processed twice. Deliveries are kept a week.

-- +goose Up
CREATE TABLE repository_sync (
    org_id          text NOT NULL DEFAULT 'default',
    repository      text NOT NULL,
    last_webhook_at timestamptz,
    last_fetch_at   timestamptz,
    last_poll_at    timestamptz,
    head            text NOT NULL DEFAULT '',
    PRIMARY KEY (org_id, repository)
);

CREATE TABLE webhook_deliveries (
    org_id      text NOT NULL DEFAULT 'default',
    repository  text NOT NULL,
    delivery_id text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, repository, delivery_id)
);

CREATE INDEX webhook_deliveries_received_at ON webhook_deliveries (received_at);

-- +goose Down
DROP TABLE webhook_deliveries;
DROP TABLE repository_sync;
