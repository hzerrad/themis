-- +goose Up
CREATE TYPE action_state AS ENUM (
  'awaiting_approval',
  'authorized',
  'executing',
  'succeeded',
  'failed',
  'ambiguous',
  'stale',
  'denied',
  'expired',
  'canceled'
  );

CREATE TABLE actions
(
  id                 uuid PRIMARY KEY,
  incident_id        uuid         NOT NULL REFERENCES incidents (id),
  proposal_id        uuid         NOT NULL REFERENCES proposals (id),
  capability         text         NOT NULL,
  capability_version text         NOT NULL,
  target_group       text         NOT NULL,
  target_kind        text         NOT NULL,
  target_namespace   text         NOT NULL,
  target_name        text         NOT NULL,
  target_uid         text         NOT NULL,
  target_generation  bigint       NOT NULL,
  target_spec_hash   text         NOT NULL,
  target_manager     text         NOT NULL,
  params             jsonb        NOT NULL,
  pre_state          jsonb        NOT NULL,
  desired_state      jsonb        NOT NULL,
  blast_radius       jsonb        NOT NULL,
  risk               text         NOT NULL,
  decision           text         NOT NULL,
  decision_reason    text         NOT NULL,
  policy_id          text         NOT NULL,
  policy_digest      text         NOT NULL,
  constraints        jsonb        NOT NULL,
  digest             text         NOT NULL,
  state              action_state NOT NULL,
  expires_at         timestamptz,
  created_at         timestamptz  NOT NULL DEFAULT now(),
  updated_at         timestamptz  NOT NULL DEFAULT now()
);

CREATE INDEX actions_incident_id_idx ON actions (incident_id);
CREATE UNIQUE INDEX actions_one_active_per_target
  ON actions (target_uid)
  WHERE state IN ('awaiting_approval', 'authorized', 'executing');

CREATE TABLE approvals
(
  id          uuid PRIMARY KEY,
  action_id   uuid        NOT NULL UNIQUE REFERENCES actions (id),
  digest      text        NOT NULL,
  approver    text        NOT NULL,
  approved_at timestamptz NOT NULL,
  consumed_at timestamptz
);

CREATE TABLE executions
(
  id              uuid PRIMARY KEY,
  action_id       uuid        NOT NULL UNIQUE REFERENCES actions (id),
  status          text        NOT NULL,
  message         text        NOT NULL,
  post_generation bigint      NOT NULL,
  started_at      timestamptz NOT NULL,
  finished_at     timestamptz NOT NULL
);

-- +goose Down
DROP TABLE executions;
DROP TABLE approvals;
DROP TABLE actions;
DROP TYPE action_state;
