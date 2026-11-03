-- +goose Up
-- Settings made in Themis's setup. A secret value is kept encrypted, with a
-- key from a Kubernetes Secret, never as written.
CREATE TABLE settings (
  name       text PRIMARY KEY,
  value      text NOT NULL,
  encrypted  boolean NOT NULL,
  updated_at timestamptz NOT NULL,
  updated_by text NOT NULL
);

-- +goose Down
DROP TABLE settings;
