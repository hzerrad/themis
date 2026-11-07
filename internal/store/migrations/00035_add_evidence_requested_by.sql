-- +goose Up
-- The person who asked for an item, when one did rather than Themis or its
-- investigator.
ALTER TABLE evidence_items ADD COLUMN requested_by text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE evidence_items DROP COLUMN requested_by;
