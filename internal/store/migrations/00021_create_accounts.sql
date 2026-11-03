-- +goose Up
-- People who sign in with a username and password, for teams without an
-- identity provider. Only the password's argon2id hash is kept.
CREATE TABLE accounts (
  username        text PRIMARY KEY,
  password_hash   text NOT NULL,
  role            text NOT NULL,
  failed_attempts integer NOT NULL DEFAULT 0,
  locked_until    timestamptz,
  created_at      timestamptz NOT NULL,
  created_by      text NOT NULL
);

-- +goose Down
DROP TABLE accounts;
