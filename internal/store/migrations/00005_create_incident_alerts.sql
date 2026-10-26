-- +goose Up
CREATE TYPE alert_status AS ENUM ('firing', 'resolved');

CREATE TABLE incident_alerts
(
  incident_id uuid         NOT NULL REFERENCES incidents (id),
  source      text         NOT NULL,
  source_id   text         NOT NULL,
  status      alert_status NOT NULL,
  starts_at   timestamptz  NOT NULL,
  ends_at     timestamptz,
  updated_at  timestamptz  NOT NULL DEFAULT now(),
  PRIMARY KEY (incident_id, source, source_id)
);

CREATE INDEX incident_alerts_source_idx ON incident_alerts (source, source_id);

-- +goose Down
DROP TABLE incident_alerts;
DROP TYPE alert_status;
