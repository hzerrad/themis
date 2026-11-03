-- +goose Up
-- What the suggester looked at, for people only. Nothing decides on it.
ALTER TABLE proposals ADD COLUMN notes text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE proposals DROP COLUMN notes;
