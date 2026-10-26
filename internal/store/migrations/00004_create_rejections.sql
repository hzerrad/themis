-- +goose Up
CREATE TYPE alert_rejection_reason AS ENUM (
  'invalid_alert',
  'not_enrolled',
  'missing_labels',
  'invalid_override',
  'unknown_kind',
  'target_not_found',
  'owner_chain_too_long',
  'no_profile',
  'processing_failed',
  'forbidden'
  );


CREATE TABLE rejected_alerts
(
  id          uuid PRIMARY KEY,
  source      text                   NOT NULL,
  received_at timestamptz            NOT NULL,
  rejected_at timestamptz            NOT NULL DEFAULT now(),
  reason      alert_rejection_reason NOT NULL,
  detail      text                   NOT NULL,
  alert_name  text                   NOT NULL DEFAULT '',
  namespace   text                   NOT NULL DEFAULT '',
  payload     jsonb                  NOT NULL
);

CREATE INDEX rejected_alerts_received_at_idx ON rejected_alerts(received_at);


-- +goose Down
DROP TABLE rejected_alerts;
DROP TYPE alert_rejection_reason;
