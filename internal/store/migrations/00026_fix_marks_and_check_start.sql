-- +goose Up
-- What each fix left, so verification and restore can recognise it without
-- knowing the fix. A rollback's mark is the template hash its approval binds.
ALTER TABLE actions ADD COLUMN mark text NOT NULL DEFAULT '';
UPDATE actions SET mark = params->>'templateHash' WHERE params ? 'templateHash';

-- Where checking began is the fix's own record.
ALTER TABLE verifications ADD COLUMN start_state jsonb NOT NULL DEFAULT '{}';
UPDATE verifications SET start_state = jsonb_build_object('ready', ready_before, 'restarts', restarts_base);
ALTER TABLE verifications DROP COLUMN ready_before, DROP COLUMN restarts_base;

-- +goose Down
ALTER TABLE verifications ADD COLUMN ready_before integer NOT NULL DEFAULT 0, ADD COLUMN restarts_base integer NOT NULL DEFAULT 0;
UPDATE verifications SET ready_before = coalesce((start_state->>'ready')::integer, 0),
                         restarts_base = coalesce((start_state->>'restarts')::integer, 0);
ALTER TABLE verifications DROP COLUMN start_state;
ALTER TABLE actions DROP COLUMN mark;
