-- +goose Up
CREATE TABLE verifications
(
  action_id     uuid PRIMARY KEY REFERENCES actions (id),
  window_start  timestamptz NOT NULL,
  ready_before  integer     NOT NULL,
  restarts_base integer     NOT NULL,
  healthy_since timestamptz,
  outcome       text        NOT NULL DEFAULT '',
  observation   jsonb       NOT NULL DEFAULT '{}',
  updated_at    timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE verifications;
