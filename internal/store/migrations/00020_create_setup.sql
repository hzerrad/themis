-- +goose Up
-- When setup finished: the first admin signed in through a real sign-in. From
-- then on, kubectl port-forward no longer lets anyone in as admin.
CREATE TABLE setup (
  only_row    boolean PRIMARY KEY DEFAULT true CHECK (only_row),
  finished_at timestamptz NOT NULL,
  finished_by text NOT NULL
);

-- An install that already has incidents or sign-ins was set up before setup
-- existed, so its door never opens.
INSERT INTO setup (finished_at, finished_by)
SELECT now(), 'an install from before setup'
WHERE EXISTS (SELECT 1 FROM incidents) OR EXISTS (SELECT 1 FROM sessions);

-- +goose Down
DROP TABLE setup;
