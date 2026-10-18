-- +goose Up
CREATE TABLE occurrences
(
  id          uuid PRIMARY KEY,
  incident_id uuid        NOT NULL REFERENCES incidents (id),
  received_at timestamptz NOT NULL DEFAULT now(),
  payload     jsonb       NOT NULL
);

CREATE INDEX occurrences_incident_id_idx ON occurrences (incident_id);

-- +goose Down
DROP TABLE occurrences;
