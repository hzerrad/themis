-- +goose Up
-- Whether an investigation that couldn't decide was missing evidence Themis
-- could have had (blind), or saw it all and found no clear answer.
ALTER TABLE investigations ADD COLUMN blind boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE investigations DROP COLUMN blind;
