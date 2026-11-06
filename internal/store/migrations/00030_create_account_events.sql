-- +goose Up
-- A password an admin sets is temporary: its holder must choose their own at
-- the next sign-in, so no admin knows a password in use.
ALTER TABLE accounts ADD COLUMN password_temporary boolean NOT NULL DEFAULT false;

-- The record of changes to accounts and of the initial admin password: who
-- did what, to which account, and when. Rows are only ever added.
CREATE TABLE account_events (
  id       bigserial PRIMARY KEY,
  at       timestamptz NOT NULL,
  actor    text NOT NULL,
  action   text NOT NULL,
  username text NOT NULL DEFAULT '',
  role     text NOT NULL DEFAULT ''
);
CREATE INDEX account_events_at ON account_events (at DESC, id DESC);

-- +goose Down
DROP TABLE account_events;
ALTER TABLE accounts DROP COLUMN password_temporary;
