-- +goose Up
-- Every other profile the incident's alert matched, highest priority first,
-- after its primary profile. An incident that matched one has none.
ALTER TABLE incidents ADD COLUMN other_profiles text[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE incidents DROP COLUMN other_profiles;
