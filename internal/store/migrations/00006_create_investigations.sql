-- +goose Up
CREATE TYPE investigation_state AS ENUM ('pending', 'leased', 'completed', 'failed');

CREATE TABLE investigations
(
  id             uuid PRIMARY KEY,
  incident_id    uuid                NOT NULL UNIQUE REFERENCES incidents (id),
  state          investigation_state NOT NULL DEFAULT 'pending',
  lease_id       uuid UNIQUE,
  lease_deadline timestamptz,
  worker_id      text                NOT NULL DEFAULT '',
  attempts       integer             NOT NULL DEFAULT 0,
  conclusion     text                NOT NULL DEFAULT '',
  summary        text                NOT NULL DEFAULT '',
  hypotheses     jsonb               NOT NULL DEFAULT '[]',
  usage          jsonb               NOT NULL DEFAULT '{}',
  provenance     jsonb               NOT NULL DEFAULT '{}',
  failure        text                NOT NULL DEFAULT '',
  created_at     timestamptz         NOT NULL DEFAULT now(),
  updated_at     timestamptz         NOT NULL DEFAULT now()
);

CREATE INDEX investigations_leasable_idx ON investigations (created_at) WHERE state IN ('pending', 'leased');

CREATE TABLE evidence_items
(
  id             uuid PRIMARY KEY,
  incident_id    uuid        NOT NULL REFERENCES incidents (id),
  lease_id       uuid,
  template       text        NOT NULL,
  params         jsonb       NOT NULL DEFAULT '{}',
  data           jsonb       NOT NULL,
  truncated      boolean     NOT NULL,
  original_bytes integer     NOT NULL,
  redactions     integer     NOT NULL,
  classification text        NOT NULL,
  collected_at   timestamptz NOT NULL
);

CREATE INDEX evidence_items_incident_id_idx ON evidence_items (incident_id, collected_at);
CREATE INDEX evidence_items_lease_id_idx ON evidence_items (lease_id);

CREATE TYPE truth AS ENUM ('true', 'false', 'unknown');

CREATE TABLE predicate_results
(
  id           bigint PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
  incident_id  uuid        NOT NULL REFERENCES incidents (id),
  name         text        NOT NULL,
  version      text        NOT NULL,
  value        truth       NOT NULL,
  reason       text        NOT NULL,
  evidence_ids uuid[]      NOT NULL,
  evaluated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX predicate_results_incident_id_idx ON predicate_results (incident_id, id);

-- +goose Down
DROP TABLE predicate_results;
DROP TYPE truth;
DROP TABLE evidence_items;
DROP TABLE investigations;
DROP TYPE investigation_state;
