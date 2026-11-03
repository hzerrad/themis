-- +goose Up
-- When each part of an install was last heard from, for the status page:
-- the investigator asking for work, Alertmanager sending alerts.
CREATE TABLE heartbeats (
  component text PRIMARY KEY,
  seen_at   timestamptz NOT NULL,
  detail    text NOT NULL DEFAULT ''
);

-- An install from before heartbeats has had alerts already: start from the
-- latest one stored, so the status page doesn't say none ever arrived.
INSERT INTO heartbeats (component, seen_at)
SELECT 'alertmanager', max(received_at) FROM occurrences HAVING max(received_at) IS NOT NULL;

-- Why an investigation attempt failed, as a fixed word, so the status page
-- can say how to fix it. failure keeps the sentence.
ALTER TABLE investigations ADD COLUMN failure_kind text;
UPDATE investigations SET failure_kind = 'stopped_responding'
WHERE failure = 'The investigator stopped responding during its last attempt';

-- +goose Down
ALTER TABLE investigations DROP COLUMN failure_kind;
DROP TABLE heartbeats;
