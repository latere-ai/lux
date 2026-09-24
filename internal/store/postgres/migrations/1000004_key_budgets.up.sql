-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

-- The Keys of one Budget, read by the Budget filter of spec 037 without
-- reading every Key: a Key names one Budget in status.budget or several
-- in status.budgets, and each form has its partial index over the live
-- Key rows.

CREATE INDEX IF NOT EXISTS objects_key_budget ON objects ((status->'budget'->>'id')) WHERE kind = 'Key' AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS objects_key_budgets ON objects USING gin ((status->'budgets') jsonb_path_ops) WHERE kind = 'Key' AND deleted_at IS NULL;
