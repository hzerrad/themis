-- +goose Up
CREATE TABLE transitions
(
  id          bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
  incident_id uuid           NOT NULL REFERENCES incidents (id),
  from_state  incident_state NOT NULL,
  to_state    incident_state NOT NULL,
  reason      text           NOT NULL,
  actor       text           NOT NULL,
  at          timestamptz    NOT NULL DEFAULT now()
);

CREATE INDEX transitions_incident_id_id_idx ON transitions (incident_id, id);

-- +goose Down
DROP TABLE transitions;
