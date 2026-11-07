-- +goose Up
-- When each sweep last looked at an incident: reconciliation, which compares
-- its target with the template it was investigated against, and the sweep
-- that decides again the symptoms of an incident waiting for a suggested fix.
-- Each looks first at the incidents it looked at longest ago, never first, so
-- past what one run looks at, every incident still comes up in turn.
ALTER TABLE incidents ADD COLUMN reconciled_at timestamptz, ADD COLUMN rechecked_at timestamptz;

-- +goose Down
ALTER TABLE incidents DROP COLUMN reconciled_at, DROP COLUMN rechecked_at;
