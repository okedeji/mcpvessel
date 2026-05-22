-- +migrate Up
CREATE TYPE cage_type AS ENUM ('discovery', 'validation', 'exploitation');
CREATE TYPE cage_state AS ENUM ('pending', 'queued', 'provisioning', 'running', 'paused', 'tearing_down', 'completed', 'failed');

CREATE TABLE cages (
    id              TEXT PRIMARY KEY,
    assessment_id   TEXT NOT NULL,
    type            cage_type NOT NULL,
    state           cage_state NOT NULL DEFAULT 'pending',
    config          JSONB NOT NULL,
    parent_finding_id TEXT,
    host_id         TEXT,
    vm_id           TEXT,
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    failure_reason  TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_cages_assessment_id ON cages(assessment_id);
CREATE INDEX idx_cages_state ON cages(state);
CREATE INDEX idx_cages_host_id ON cages(host_id);

-- +migrate Down
DROP TABLE IF EXISTS cages;
DROP TYPE IF EXISTS cage_state;
DROP TYPE IF EXISTS cage_type;
