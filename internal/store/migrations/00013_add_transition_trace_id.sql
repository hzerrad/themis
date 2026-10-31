-- +goose Up
-- The trace of the work that made the change, to find it from the audit trail.
ALTER TABLE transitions ADD COLUMN trace_id text;

-- +goose Down
ALTER TABLE transitions DROP COLUMN trace_id;
