-- +goose Up
CREATE TABLE alert_resolutions
(
  source     text        NOT NULL,
  source_id  text        NOT NULL,
  starts_at  timestamptz NOT NULL,
  ends_at    timestamptz NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (source, source_id)
);

-- +goose Down
DROP TABLE alert_resolutions;
