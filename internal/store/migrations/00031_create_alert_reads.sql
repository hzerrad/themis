-- +goose Up
-- How Themis last read each source it reads alerts from, such as
-- Alertmanager's API: for the status page, and so a read that finished after a
-- later one changes nothing. When the address or matchers change, what was
-- read before is forgotten, never resolved.
CREATE TABLE alert_reads (
  source       text PRIMARY KEY,
  address      text NOT NULL DEFAULT '',
  matchers     text NOT NULL DEFAULT '',
  read_at      timestamptz,
  failed_at    timestamptz,
  failure_kind text NOT NULL DEFAULT '',
  failure      text NOT NULL DEFAULT ''
);

-- The alerts Themis took from a source it reads, while the source lists them:
-- an alert that leaves the list has stopped firing.
CREATE TABLE read_alerts (
  source    text        NOT NULL,
  source_id text        NOT NULL,
  name      text        NOT NULL,
  starts_at timestamptz NOT NULL,
  ends_at   timestamptz NOT NULL,
  seen_at   timestamptz NOT NULL,
  taken_at  timestamptz NOT NULL,
  PRIMARY KEY (source, source_id)
);

-- +goose Down
DROP TABLE read_alerts;
DROP TABLE alert_reads;
