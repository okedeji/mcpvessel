-- +migrate Up
CREATE TYPE intervention_type AS ENUM ('tripwire_escalation', 'payload_review', 'report_review', 'policy_violation', 'agent_hold', 'plan_approval');
CREATE TYPE intervention_status AS ENUM ('pending', 'resolved', 'timed_out');
CREATE TYPE intervention_priority AS ENUM ('low', 'medium', 'high', 'critical');

CREATE TABLE interventions (
    id              TEXT PRIMARY KEY,
    type            intervention_type NOT NULL,
    status          intervention_status NOT NULL DEFAULT 'pending',
    priority        intervention_priority NOT NULL DEFAULT 'medium',
    cage_id         TEXT,
    assessment_id   TEXT NOT NULL REFERENCES assessments(id),
    description     TEXT NOT NULL,
    context_data    JSONB,
    timeout_at      TIMESTAMPTZ,
    operator_id     TEXT,
    action          TEXT,
    rationale       TEXT,
    adjustments     JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at     TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_interventions_status ON interventions(status);
CREATE INDEX idx_interventions_assessment_id ON interventions(assessment_id);
CREATE INDEX idx_interventions_cage_id ON interventions(cage_id);
CREATE INDEX idx_interventions_status_priority ON interventions(status, priority);

-- +migrate Down
DROP TABLE IF EXISTS interventions;
DROP TYPE IF EXISTS intervention_priority;
DROP TYPE IF EXISTS intervention_status;
DROP TYPE IF EXISTS intervention_type;
