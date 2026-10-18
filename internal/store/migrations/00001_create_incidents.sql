-- +goose Up
CREATE TYPE incident_state as enum (
  'received',
  'investigating',
  'diagnosed',
  'inconclusive',
  'recommended',
  'proposed',
  'awaiting_approval',
  'authorized',
  'executing',
  'verifying',
  'resolved',
  'contained_escalated',
  'escalated',
  'closed_no_action',
  'self_resolved',
  'rejected',
  'canceled'
  );

CREATE TABLE incidents
(
  id               uuid PRIMARY KEY,
  fingerprint      text           NOT NULL,
  state            incident_state NOT NULL,
  profile          text           NOT NULL,
  target_group     text           NOT NULL,
  target_kind      text           NOT NULL,
  target_namespace text           NOT NULL,
  target_name      text           NOT NULL,
  target_uid       text           NOT NULL,
  occurrence_count integer        NOT NULL DEFAULT 1,
  created_at       timestamptz    NOT NULL DEFAULT now(),
  updated_at       timestamptz    NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX incidents_open_fingerprint
  ON incidents (fingerprint)
  WHERE state NOT IN
        ('resolved', 'contained_escalated', 'escalated', 'closed_no_action', 'self_resolved', 'rejected', 'canceled');

-- +goose Down
DROP TABLE incidents;
DROP TYPE incident_state;
