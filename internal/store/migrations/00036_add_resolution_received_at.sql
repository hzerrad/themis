-- +goose Up
-- When the resolution of an alert's episode was received. A firing
-- notification of that episode received before it is late; one received after
-- it means the episode goes on, as when Alertmanager let the alert's end pass
-- while Prometheus couldn't send it, then got it again. Resolutions recorded
-- before this column count as received when they were recorded.
ALTER TABLE alert_resolutions ADD COLUMN received_at timestamptz;
UPDATE alert_resolutions SET received_at = updated_at;
ALTER TABLE alert_resolutions ALTER COLUMN received_at SET NOT NULL;

-- +goose Down
ALTER TABLE alert_resolutions DROP COLUMN received_at;
