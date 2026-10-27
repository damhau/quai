-- The first schema: requests and what they propose, reviews, events, runs, sessions and API
-- tokens (docs/architecture/domain-model.md). Times are written by PostgreSQL, never by the
-- engine. Top-level rows carry an org_id, one organization per installation for now, so that
-- several organizations need no migration later (docs/design/08-identity-access-tenancy.md).

-- +goose Up
CREATE TABLE requests (
    id                 uuid PRIMARY KEY,
    org_id             text NOT NULL DEFAULT 'default',
    kind               text NOT NULL CHECK (kind IN ('create', 'update', 'delete', 'action')),
    action             text NOT NULL DEFAULT '',
    -- What the request changes: a resource ID (<repository>/<type>/<key>) or a blueprint
    -- instance (<blueprint>/<instance>).
    target             text NOT NULL,
    type_name          text NOT NULL DEFAULT '',
    blueprint          text NOT NULL DEFAULT '',
    requester          text NOT NULL,
    reason             text NOT NULL DEFAULT '',
    inputs             jsonb NOT NULL DEFAULT '{}',
    state              text NOT NULL CHECK (state IN (
        'planned', 'pending_approval', 'approved', 'rejected', 'cancelled', 'expired',
        'stale', 'merging', 'applying', 'applied', 'failed')),
    -- The commits the request was planned at: definitions and base of the change.
    definitions_commit text NOT NULL,
    base_commit        text NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX requests_state ON requests (org_id, state, created_at);
CREATE INDEX requests_requester ON requests (org_id, requester, created_at);
-- At most one open request per target: further requests wait until it completes.
CREATE UNIQUE INDEX requests_one_open_per_target ON requests (org_id, target)
    WHERE state IN ('planned', 'pending_approval', 'approved', 'stale', 'merging', 'applying');

-- One change set per repository a request writes to.
CREATE TABLE change_sets (
    id           uuid PRIMARY KEY,
    request_id   uuid NOT NULL REFERENCES requests (id) ON DELETE CASCADE,
    repository   text NOT NULL,
    hash         text NOT NULL,
    fingerprints jsonb NOT NULL DEFAULT '{}', -- region path → fingerprint at the base commit
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (request_id, repository)
);

CREATE TABLE changes (
    change_set_id uuid NOT NULL REFERENCES change_sets (id) ON DELETE CASCADE,
    path          text NOT NULL,
    op            text NOT NULL CHECK (op IN ('create', 'update', 'delete')),
    before        bytea,
    after         bytea,
    PRIMARY KEY (change_set_id, path)
);

-- A review is bound to the change set hash it was given for.
CREATE TABLE reviews (
    id              uuid PRIMARY KEY,
    request_id      uuid NOT NULL REFERENCES requests (id) ON DELETE CASCADE,
    reviewer        text NOT NULL,
    groups          text[] NOT NULL DEFAULT '{}',
    decision        text NOT NULL CHECK (decision IN ('approve', 'deny')),
    comment         text NOT NULL DEFAULT '',
    change_set_hash text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX reviews_request ON reviews (request_id, created_at);

-- What happened to requests and resources, for timelines. The hash-chained audit log is
-- separate (Phase 4).
CREATE TABLE events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id     text NOT NULL DEFAULT 'default',
    request_id uuid REFERENCES requests (id) ON DELETE CASCADE,
    target     text NOT NULL DEFAULT '',
    kind       text NOT NULL,
    actor      text NOT NULL DEFAULT '',
    data       jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_request ON events (request_id, id);
CREATE INDEX events_target ON events (org_id, target, id);

-- Plans, applies, syncs and drift checks reported by runners.
CREATE TABLE runs (
    id          uuid PRIMARY KEY,
    org_id      text NOT NULL DEFAULT 'default',
    repository  text NOT NULL,
    stack       text NOT NULL,
    commit      text NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('plan', 'apply', 'sync', 'drift')),
    status      text NOT NULL CHECK (status IN ('running', 'succeeded', 'failed')),
    url         text NOT NULL DEFAULT '',
    summary     jsonb NOT NULL DEFAULT '{}',
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);
CREATE INDEX runs_stack ON runs (org_id, repository, stack, started_at DESC);

-- Server-side sessions, in the layout alexedwards/scs's PostgreSQL store expects.
CREATE TABLE sessions (
    token  text PRIMARY KEY,
    data   bytea NOT NULL,
    expiry timestamptz NOT NULL
);
CREATE INDEX sessions_expiry ON sessions (expiry);

-- API tokens are stored as SHA-256 hashes; the token itself is shown once. A token acts with
-- the groups its owner had when creating it, narrowed by its scopes.
CREATE TABLE api_tokens (
    id           uuid PRIMARY KEY,
    org_id       text NOT NULL DEFAULT 'default',
    owner        text NOT NULL,
    groups       text[] NOT NULL DEFAULT '{}',
    name         text NOT NULL,
    hash         bytea NOT NULL UNIQUE,
    scopes       text[] NOT NULL,
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz,
    UNIQUE (org_id, owner, name)
);

-- +goose Down
DROP TABLE api_tokens;
DROP TABLE sessions;
DROP TABLE runs;
DROP TABLE events;
DROP TABLE reviews;
DROP TABLE changes;
DROP TABLE change_sets;
DROP TABLE requests;
