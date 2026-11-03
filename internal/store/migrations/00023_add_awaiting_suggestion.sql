-- +goose Up
-- Without an investigator, an incident waits for a fix suggested by a person
-- or a tool outside Themis.
ALTER TYPE incident_state ADD VALUE 'awaiting_suggestion' AFTER 'investigating';

-- +goose Down
-- Postgres can't drop an enum value; an unused one is harmless.
SELECT 1;
