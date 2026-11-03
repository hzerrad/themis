-- +goose Up
-- When setup finished: the first admin signed in through a real sign-in. From
-- then on, kubectl port-forward no longer lets anyone in as admin.
CREATE TABLE setup (
  only_row    boolean PRIMARY KEY DEFAULT true CHECK (only_row),
  finished_at timestamptz NOT NULL,
  finished_by text NOT NULL
);

-- +goose Down
DROP TABLE setup;
