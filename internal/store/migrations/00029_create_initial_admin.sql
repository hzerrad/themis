-- +goose Up
-- Wrong initial admin passwords, counted as an account's are: too many in a
-- row lock it for a while. The password itself is never stored here; Themis
-- reads it from the Secret the chart generates. One row, always.
CREATE TABLE initial_admin (
  only_row        boolean PRIMARY KEY DEFAULT true CHECK (only_row),
  failed_attempts integer NOT NULL DEFAULT 0,
  locked_until    timestamptz
);
INSERT INTO initial_admin DEFAULT VALUES;

-- +goose Down
DROP TABLE initial_admin;
