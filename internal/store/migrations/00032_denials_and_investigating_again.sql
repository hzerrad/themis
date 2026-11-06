-- +goose Up
-- A person's denial of a fix, with their reason exactly as they wrote it.
CREATE TABLE denials
(
  action_id uuid PRIMARY KEY REFERENCES actions (id),
  denier    text        NOT NULL,
  reason    text        NOT NULL,
  denied_at timestamptz NOT NULL
);

-- After denying its fix, a person can have an incident investigated again:
-- each time is an investigation of its own.
ALTER TABLE investigations DROP CONSTRAINT investigations_incident_id_key;
CREATE INDEX investigations_incident_id_idx ON investigations (incident_id, created_at);

-- +goose Down
-- An incident investigated more than once keeps its first investigation only.
DELETE FROM investigations v
WHERE EXISTS (SELECT 1 FROM investigations f WHERE f.incident_id = v.incident_id AND f.created_at < v.created_at);
DROP INDEX investigations_incident_id_idx;
ALTER TABLE investigations ADD CONSTRAINT investigations_incident_id_key UNIQUE (incident_id);
DROP TABLE denials;
