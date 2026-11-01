-- +goose Up
-- Putting back what Themis changed, after a fix that didn't help.
ALTER TYPE incident_state ADD VALUE 'restoring' AFTER 'verifying';

-- +goose Down
-- An enum value cannot be dropped; a database that has it keeps it.
SELECT 1;
