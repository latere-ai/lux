-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- The aggregates of spec 009: one row per hour per dimension tuple, the
-- Key's labels denormalised, the sums added on conflict. The primary key
-- is every dimension with the bucket, which is what the upsert lands on;
-- the bucket index serves the range query.

CREATE TABLE IF NOT EXISTS usage_hourly (
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

CREATE INDEX IF NOT EXISTS usage_hourly_bucket ON usage_hourly (bucket);
