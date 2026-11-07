-- +goose Up
-- Each check's answer in sentences for people, when its check writes them:
-- a fact for the top of the incident's page, and a headline for a check that
-- finds the failure. Investigators never see either.
ALTER TABLE predicate_results ADD COLUMN fact text NOT NULL DEFAULT '';
ALTER TABLE predicate_results ADD COLUMN headline text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE predicate_results DROP COLUMN headline;
ALTER TABLE predicate_results DROP COLUMN fact;
