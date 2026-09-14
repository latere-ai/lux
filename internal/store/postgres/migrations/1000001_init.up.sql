-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- The first migration of the v1 schema: desired state, the door's
-- lookup, the sealed credentials, the spend windows, the leases, the
-- journal, and the tunnel registry. Within a major a migration only
-- creates a table, adds a nullable or defaulted column, or adds an
-- index, so the previous minor's binary keeps reading every row during a
-- roll and after an undo; TestMigrationsAreAdditive holds every file in
-- this directory to that grammar.

CREATE TABLE IF NOT EXISTS objects (
    kind       text        NOT NULL,
    id         text        PRIMARY KEY,
    name       text        NOT NULL,
    owner      text        NOT NULL DEFAULT '',
    source     text        NOT NULL DEFAULT '',
    version    bigint      NOT NULL,
    spec       jsonb       NOT NULL,
    status     jsonb       NOT NULL DEFAULT '{}',
    observed   jsonb       NOT NULL DEFAULT '{}',
    labels     jsonb       NOT NULL DEFAULT '{}',
    providers  text[]      NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    deleted_at timestamptz
);

CREATE UNIQUE INDEX IF NOT EXISTS objects_live_name ON objects (kind, name) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS objects_live_owner ON objects (kind, owner) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS objects_live_providers ON objects USING gin (providers) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS objects_live_source ON objects (kind, source) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS objects_labels ON objects USING gin (labels);
CREATE INDEX IF NOT EXISTS objects_deleted ON objects (deleted_at) WHERE deleted_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS key_hashes (
    hash       text        PRIMARY KEY,
    key_id     text        NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS credentials (
    provider_id   text        PRIMARY KEY,
    version       integer     NOT NULL,
    wrapped_key   bytea       NOT NULL,
    wrapped_nonce bytea       NOT NULL,
    ciphertext    bytea       NOT NULL,
    nonce         bytea       NOT NULL,
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS counters (
    key        text   PRIMARY KEY,
    value      bigint NOT NULL DEFAULT 0,
    expires_at timestamptz
);

CREATE INDEX IF NOT EXISTS counters_expires ON counters (expires_at) WHERE expires_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS leases (
    name       text        PRIMARY KEY,
    holder     text        NOT NULL,
    expires_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS journal (
    id              text        PRIMARY KEY,
    gseq            bigint      NOT NULL,
    object_id       text        NOT NULL,
    seq             bigint      NOT NULL,
    type            text        NOT NULL DEFAULT '',
    at              timestamptz NOT NULL,
    payload         bytea,
    attempts        integer     NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL,
    acked_at        timestamptz
);

CREATE INDEX IF NOT EXISTS journal_object_seq ON journal (object_id, seq);
CREATE INDEX IF NOT EXISTS journal_gseq ON journal (gseq);
CREATE INDEX IF NOT EXISTS journal_due ON journal (next_attempt_at) WHERE acked_at IS NULL;

CREATE TABLE IF NOT EXISTS tunnels (
    provider_id  text        PRIMARY KEY,
    session      text        NOT NULL,
    replica      text        NOT NULL DEFAULT '',
    subject      text        NOT NULL DEFAULT '',
    agent        text        NOT NULL DEFAULT '',
    connected_at timestamptz NOT NULL,
    expires_at   timestamptz NOT NULL
);
