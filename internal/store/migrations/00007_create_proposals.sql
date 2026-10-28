-- +goose Up
CREATE TYPE proposal_status AS ENUM ('accepted', 'rejected', 'superseded', 'deferred');

CREATE TABLE proposals
(
  id                 uuid PRIMARY KEY,
  incident_id        uuid            NOT NULL REFERENCES incidents (id),
  lease_id           uuid            NOT NULL,
  client_proposal_id text            NOT NULL,
  capability         text            NOT NULL,
  capability_version text            NOT NULL,
  target_group       text            NOT NULL,
  target_kind        text            NOT NULL,
  target_namespace   text            NOT NULL,
  target_name        text            NOT NULL,
  target_uid         text            NOT NULL,
  params             jsonb           NOT NULL,
  expected_effect    text            NOT NULL,
  rationale          text            NOT NULL,
  cited_evidence_ids text[]          NOT NULL,
  cited_predicates   text[]          NOT NULL,
  provenance         jsonb           NOT NULL,
  authenticated      boolean         NOT NULL,
  status             proposal_status NOT NULL,
  rejection_reason   text            NOT NULL DEFAULT '',
  message            text            NOT NULL DEFAULT '',
  created_at         timestamptz     NOT NULL DEFAULT now(),
  UNIQUE (lease_id, client_proposal_id)
);

CREATE INDEX proposals_incident_id_idx ON proposals (incident_id, created_at);

CREATE TABLE safety_events
(
  id          bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
  incident_id uuid        NOT NULL REFERENCES incidents (id),
  proposal_id uuid REFERENCES proposals (id),
  kind        text        NOT NULL,
  detail      text        NOT NULL,
  at          timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX safety_events_incident_id_idx ON safety_events (incident_id, id);

-- +goose Down
DROP TABLE safety_events;
DROP TABLE proposals;
DROP TYPE proposal_status;
