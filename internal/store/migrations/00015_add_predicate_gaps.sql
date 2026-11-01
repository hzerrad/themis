-- +goose Up
-- The missing evidence behind an unknown result, and whether Themis could not
-- see it (blind) or saw it and it did not settle the question.
ALTER TABLE predicate_results ADD COLUMN gaps jsonb NOT NULL DEFAULT '[]';
ALTER TABLE predicate_results ADD COLUMN blind boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE predicate_results DROP COLUMN blind;
ALTER TABLE predicate_results DROP COLUMN gaps;
