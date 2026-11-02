-- +goose Up
-- Sign-ins under way: the state the identity provider must hand back.
CREATE TABLE sign_ins (
  state      text PRIMARY KEY,
  nonce      text NOT NULL,
  verifier   text NOT NULL,
  expires_at timestamptz NOT NULL
);

-- Signed-in people. The cookie holds the ID; only its hash is kept.
CREATE TABLE sessions (
  id_hash    bytea PRIMARY KEY,
  subject    text NOT NULL,
  username   text NOT NULL,
  groups     text[] NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL
);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- +goose Down
DROP TABLE sessions;
DROP TABLE sign_ins;
