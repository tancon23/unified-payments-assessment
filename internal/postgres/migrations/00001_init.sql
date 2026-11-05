-- +goose Up
CREATE TABLE tenants (
    id           text PRIMARY KEY,
    name         text NOT NULL,
    api_key_hash bytea NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE payments (
    id               text PRIMARY KEY,
    tenant_id        text NOT NULL REFERENCES tenants (id),
    idempotency_key  text NOT NULL,
    request_hash     bytea NOT NULL,
    provider         text NOT NULL,
    mode             text NOT NULL CHECK (mode IN ('payment', 'subscription')),
    amount_minor     bigint NOT NULL CHECK (amount_minor > 0),
    currency         text NOT NULL CHECK (currency ~ '^[a-z]{3}$'),
    billing_interval text NOT NULL DEFAULT '',
    description      text NOT NULL,
    success_url      text NOT NULL,
    cancel_url       text NOT NULL,
    status           text NOT NULL CHECK (status IN (
                         'pending', 'unknown', 'authorized', 'succeeded', 'failed',
                         'expired', 'partially_refunded', 'refunded')),
    -- The last line of defence against counting a refund twice.
    refunded_minor   bigint NOT NULL DEFAULT 0 CHECK (refunded_minor BETWEEN 0 AND amount_minor),
    checkout_ref     text,
    checkout_url     text,
    payment_ref      text,
    failure_reason   text NOT NULL DEFAULT '',
    -- Provider calls that got no answer for the current step, and whether
    -- the payment ran out of attempts and waits for a person.
    attempts         integer NOT NULL DEFAULT 0,
    flagged          boolean NOT NULL DEFAULT false,
    flag_reason      text NOT NULL DEFAULT '',
    claimed_by       text,
    claimed_until    timestamptz,
    next_check_at    timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, idempotency_key),
    UNIQUE (id, tenant_id)
);

CREATE UNIQUE INDEX payments_checkout_ref ON payments (provider, checkout_ref) WHERE checkout_ref IS NOT NULL;
CREATE INDEX payments_due ON payments (next_check_at)
    WHERE status IN ('pending', 'unknown', 'authorized') AND NOT flagged;

CREATE TABLE refunds (
    id               text PRIMARY KEY,
    payment_id       text NOT NULL,
    tenant_id        text NOT NULL,
    idempotency_key  text NOT NULL,
    request_hash     bytea NOT NULL,
    amount_minor     bigint NOT NULL CHECK (amount_minor > 0),
    status           text NOT NULL CHECK (status IN ('unknown', 'pending', 'succeeded', 'failed')),
    provider_ref     text,
    failure_reason   text NOT NULL DEFAULT '',
    attempts         integer NOT NULL DEFAULT 0,
    flagged          boolean NOT NULL DEFAULT false,
    flag_reason      text NOT NULL DEFAULT '',
    claimed_by       text,
    claimed_until    timestamptz,
    next_check_at    timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, idempotency_key),
    -- A refund always belongs to the same tenant as its payment.
    FOREIGN KEY (payment_id, tenant_id) REFERENCES payments (id, tenant_id)
);

CREATE INDEX refunds_payment ON refunds (payment_id);
CREATE INDEX refunds_provider_ref ON refunds (provider_ref) WHERE provider_ref IS NOT NULL;
CREATE INDEX refunds_due ON refunds (next_check_at) WHERE status IN ('unknown', 'pending') AND NOT flagged;

-- One row per provider event ever processed: the primary key is what makes
-- a redelivered webhook a no-op.
CREATE TABLE webhook_events (
    provider    text NOT NULL,
    event_id    text NOT NULL,
    event_type  text NOT NULL,
    outcome     text NOT NULL DEFAULT '',
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, event_id)
);

-- +goose Down
DROP TABLE webhook_events;
DROP TABLE refunds;
DROP TABLE payments;
DROP TABLE tenants;
