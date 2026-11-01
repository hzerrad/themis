-- +goose Up
ALTER TABLE proposals ALTER COLUMN lease_id DROP NOT NULL;
CREATE UNIQUE INDEX proposals_external_once_idx ON proposals (incident_id, (provenance ->> 'source'), client_proposal_id)
  WHERE lease_id IS NULL;

-- +goose Down
DROP INDEX proposals_external_once_idx;
DELETE FROM safety_events WHERE proposal_id IN (SELECT id FROM proposals WHERE lease_id IS NULL);
DELETE FROM proposals WHERE lease_id IS NULL;
ALTER TABLE proposals ALTER COLUMN lease_id SET NOT NULL;
