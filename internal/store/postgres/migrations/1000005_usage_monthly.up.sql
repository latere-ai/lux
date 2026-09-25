-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- The monthly rows of spec 038: the hourly rows a retention rule has
-- folded, one row per month per dimension tuple, with the columns and
-- the primary key of usage_hourly, the bucket the first instant of the
-- month in UTC. A table of its own, because the primary key of
-- usage_hourly is not a migration's to change.

CREATE TABLE IF NOT EXISTS usage_monthly (
    bucket              timestamptz NOT NULL,
    key_id              text        NOT NULL DEFAULT '',
    model_id            text        NOT NULL DEFAULT '',
    provider_id         text        NOT NULL DEFAULT '',
    owner               text        NOT NULL DEFAULT '',
    door                text        NOT NULL DEFAULT '',
    status              text        NOT NULL DEFAULT '',
    currency            text        NOT NULL DEFAULT '',
    labels              jsonb       NOT NULL DEFAULT '{}',
    requests            bigint      NOT NULL DEFAULT 0,
    input_tokens        bigint      NOT NULL DEFAULT 0,
    output_tokens       bigint      NOT NULL DEFAULT 0,
    cached_input_tokens bigint      NOT NULL DEFAULT 0,
    cache_write_tokens  bigint      NOT NULL DEFAULT 0,
    cost_micro          bigint      NOT NULL DEFAULT 0,
    unpriced            bigint      NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket, key_id, model_id, provider_id, owner, door, status, currency)
);

-- The owner a redaction rewrites, in both tables.
CREATE INDEX IF NOT EXISTS usage_hourly_owner ON usage_hourly (owner) WHERE owner <> '';

CREATE INDEX IF NOT EXISTS usage_monthly_owner ON usage_monthly (owner) WHERE owner <> '';
