-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

CREATE TABLE IF NOT EXISTS key_fences (
    name text PRIMARY KEY,
    owner text NOT NULL,
    labels jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL
);
