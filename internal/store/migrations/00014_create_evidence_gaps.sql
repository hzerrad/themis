-- +goose Up
-- Evidence an investigation needed and could not have, with why.
CREATE TABLE evidence_gaps
(
  id          bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
  incident_id uuid        NOT NULL REFERENCES incidents (id),
  lease_id    uuid,
  template    text        NOT NULL,
  params      jsonb       NOT NULL DEFAULT '{}',
  cause       text        NOT NULL CHECK (cause IN ('forbidden', 'not_in_profile', 'expired', 'not_found', 'budget_spent',
                                                    'data_ceiling', 'timeout', 'unavailable', 'absent')),
  detail      text        NOT NULL DEFAULT '',
  at          timestamptz NOT NULL
);

CREATE INDEX evidence_gaps_incident_id_idx ON evidence_gaps (incident_id, id);

-- One gap per lease, template and cause: a repeated refusal says nothing new,
-- and an investigator cannot fill the table by asking again.
CREATE UNIQUE INDEX evidence_gaps_once_idx ON evidence_gaps (incident_id, lease_id, template, cause) NULLS NOT DISTINCT;

-- +goose Down
DROP TABLE evidence_gaps;
