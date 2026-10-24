-- +goose Up
-- Every change a merchant may want to react to is an event, written in the same transaction as the
-- change itself (a transactional outbox): an event exists if and only if its change committed.
CREATE TABLE events (
    id          text        PRIMARY KEY,                       -- evt_...
    -- seq is the order events were committed in, per merchant (Record serializes them per merchant).
    seq         bigint      GENERATED ALWAYS AS IDENTITY UNIQUE,
    merchant_id bigint      NOT NULL REFERENCES merchants (id),
    livemode    boolean     NOT NULL,
    type        text        NOT NULL,                          -- payment_intent.succeeded, ...
    api_version text        NOT NULL,                          -- the shape data.object is rendered in
    data        jsonb       NOT NULL CHECK (jsonb_typeof(data) = 'object'),
    -- clock_timestamp, not now(): taken after Record's lock, so it follows commit order.
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX events_owner_created_idx ON events (merchant_id, livemode, created_at DESC, id DESC);

CREATE TABLE webhook_endpoints (
    id             text        PRIMARY KEY,                    -- we_...
    merchant_id    bigint      NOT NULL REFERENCES merchants (id),
    livemode       boolean     NOT NULL,
    url            text        NOT NULL,
    -- The signing secret is kept in clear: signing needs it, and only a hash would make that impossible.
    secret         text        NOT NULL,
    description    text,
    enabled_events text[]      NOT NULL CHECK (cardinality(enabled_events) > 0),
    status         text        NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    deleted_at     timestamptz
);

CREATE INDEX webhook_endpoints_owner_idx ON webhook_endpoints (merchant_id, livemode, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;

-- One row per (event, endpoint) that subscribed to it when the event was recorded. Deliveries are
-- at least once: a row stays pending until the endpoint answers 2xx or three days have passed.
CREATE TABLE webhook_deliveries (
    id                   bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id             text        NOT NULL REFERENCES events (id),
    endpoint_id          text        NOT NULL REFERENCES webhook_endpoints (id),
    status               text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'succeeded', 'failed')),
    attempts             integer     NOT NULL DEFAULT 0,
    next_attempt_at      timestamptz NOT NULL DEFAULT now(),
    -- Set while a dispatcher is sending it; another dispatcher may take it over once this has passed.
    locked_until         timestamptz,
    last_attempt_at      timestamptz,
    last_response_status integer,
    last_error           text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (event_id, endpoint_id)
);

CREATE INDEX webhook_deliveries_pending_idx ON webhook_deliveries (endpoint_id, id) WHERE status = 'pending';

-- +goose Down
DROP TABLE webhook_deliveries;
DROP TABLE webhook_endpoints;
DROP TABLE events;
