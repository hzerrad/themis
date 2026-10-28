-- +goose Up
ALTER TABLE investigations ADD COLUMN lease_after timestamptz NOT NULL DEFAULT 'epoch';

-- +goose Down
ALTER TABLE investigations DROP COLUMN lease_after;
