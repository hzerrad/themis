-- +goose Up
-- Text in each item that looks written for the investigator, such as
-- instructions or forged tool output, with why and where. Most items have none.
ALTER TABLE evidence_items ADD COLUMN suspicious jsonb NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE evidence_items DROP COLUMN suspicious;
