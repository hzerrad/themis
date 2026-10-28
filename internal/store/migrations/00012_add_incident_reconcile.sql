-- +goose Up
ALTER TABLE incidents ADD COLUMN observed_spec_hash text NOT NULL DEFAULT '';
ALTER TABLE incidents ADD COLUMN superseded_by uuid REFERENCES incidents (id);

-- +goose Down
ALTER TABLE incidents DROP COLUMN superseded_by;
ALTER TABLE incidents DROP COLUMN observed_spec_hash;
