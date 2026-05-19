-- +migrate Up
CREATE TYPE assessment_status AS ENUM ('discovery', 'awaiting_plan_approval', 'exploitation', 'validation', 'pending_review', 'approved', 'rejected', 'unreviewed', 'plan_unapproved', 'failed');

CREATE TABLE assessments (
    id              TEXT PRIMARY KEY,
    customer_id     TEXT NOT NULL,
    status          assessment_status NOT NULL DEFAULT 'discovery',
    config          JSONB NOT NULL,
    total_cages     INTEGER NOT NULL DEFAULT 0,
    active_cages    INTEGER NOT NULL DEFAULT 0,
    findings_candidate  INTEGER NOT NULL DEFAULT 0,
    findings_validated  INTEGER NOT NULL DEFAULT 0,
    findings_rejected   INTEGER NOT NULL DEFAULT 0,
    tokens_consumed BIGINT NOT NULL DEFAULT 0,
    report          JSONB,
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_assessments_customer_id ON assessments(customer_id);
CREATE INDEX idx_assessments_status ON assessments(status);

ALTER TABLE cages ADD CONSTRAINT fk_cages_assessment FOREIGN KEY (assessment_id) REFERENCES assessments(id);

-- +migrate Down
ALTER TABLE cages DROP CONSTRAINT IF EXISTS fk_cages_assessment;
DROP TABLE IF EXISTS assessments;
DROP TYPE IF EXISTS assessment_status;
