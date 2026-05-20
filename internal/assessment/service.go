package assessment

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/ids"
	"github.com/okedeji/agentcage/internal/plan"
)

const TaskQueue = "assessment-lifecycle"

var ErrAssessmentNotFound = errors.New("assessment not found")

// FleetSignaler notifies the fleet about assessment lifecycle events.
// Defined as an interface to avoid importing the fleet package directly.
type FleetSignaler interface {
	OnNewAssessment(assessmentID string, surfaceSize int)
	OnAssessmentComplete(assessmentID string)
}

type Service struct {
	temporal    client.Client
	db          *sql.DB
	fleet       FleetSignaler
	operatorCfg *config.Config
	mu          sync.RWMutex
	assessments map[string]*Info
}

func NewService(temporal client.Client, db *sql.DB, fleet FleetSignaler, operatorCfg *config.Config) *Service {
	return &Service{
		temporal:    temporal,
		db:          db,
		fleet:       fleet,
		operatorCfg: operatorCfg,
		assessments: make(map[string]*Info),
	}
}

func (s *Service) CreateAssessment(ctx context.Context, cfg Config) (*Info, error) {
	// Same merge order as the CLI: operator defaults → request overrides →
	// apply defaults → validate → enforce ceilings. SDK users who don't
	// set tokenBudget get the operator's default instead of zero.
	var basePlan *plan.Plan
	if s.operatorCfg != nil {
		basePlan = plan.BasePlanFromConfig(s.operatorCfg)
	} else {
		basePlan = &plan.Plan{}
	}
	incoming := configToPlan(cfg)
	p := plan.Merge(basePlan, incoming)

	if s.operatorCfg != nil {
		plan.ResolveDefaults(p, s.operatorCfg)
	}
	plan.ApplyDefaults(p)
	if err := plan.Validate(p); err != nil {
		return nil, fmt.Errorf("invalid assessment config: %w", err)
	}
	if s.operatorCfg != nil {
		if err := plan.EnforceConfigCeilings(p, s.operatorCfg); err != nil {
			return nil, fmt.Errorf("assessment config exceeds operator limits: %w", err)
		}
	}

	assessmentID := ids.Assessment()
	now := time.Now()
	info := &Info{
		ID:         assessmentID,
		CustomerID: cfg.CustomerID,
		Status:     StatusDiscovery,
		Config:     cfg,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := s.persistAssessment(ctx, info); err != nil {
		return nil, fmt.Errorf("persisting assessment %s: %w", assessmentID, err)
	}

	s.mu.Lock()
	s.assessments[assessmentID] = info
	s.mu.Unlock()

	// Write resolved limits back into cfg so the workflow sees them.
	cfg.MaxIterations = p.Limits.MaxIterations
	cfg.MaxTotalCages = p.Limits.MaxTotalCages
	cfg.TokenBudget = p.Budget.Tokens

	// Pipe orchestrator's judge config into the assessment so
	// workflow-spawned cages can wire judge into their payload-proxy.
	// Skipped when the operator opted out via --no-judge.
	if s.operatorCfg != nil && !cfg.NoJudge {
		cfg.JudgeEndpoint = s.operatorCfg.JudgeEndpoint()
		cfg.JudgeConfidence = s.operatorCfg.JudgeConfidenceThreshold()
		cfg.JudgeTimeoutSec = int(s.operatorCfg.JudgeTimeout().Seconds())
	}

	// Fill rate limits from operator config into CageDefaults.
	if s.operatorCfg != nil {
		if cfg.CageDefaults == nil {
			cfg.CageDefaults = make(map[cage.Type]CageTypeConfig)
		}
		for name, opCfg := range s.operatorCfg.Cages {
			t := cage.TypeFromString(name)
			if t == cage.TypeUnspecified {
				continue
			}
			tc := cfg.CageDefaults[t]
			tc.Type = t
			if tc.RateLimit <= 0 && opCfg.RateLimit > 0 {
				tc.RateLimit = opCfg.RateLimit
			}
			if tc.MaxDuration <= 0 && opCfg.MaxDuration > 0 {
				tc.MaxDuration = opCfg.MaxDuration
			}
			if tc.Resources.VCPUs <= 0 && opCfg.DefaultVCPUs > 0 {
				tc.Resources = cage.ResourceLimits{VCPUs: opCfg.DefaultVCPUs, MemoryMB: opCfg.DefaultMemoryMB}
			}
			cfg.CageDefaults[t] = tc
		}
	}

	workflowOpts := client.StartWorkflowOptions{
		ID:        assessmentID,
		TaskQueue: TaskQueue,
	}
	input := AssessmentWorkflowInput{
		AssessmentID: assessmentID,
		Config:       cfg,
	}

	if _, err := s.temporal.ExecuteWorkflow(ctx, workflowOpts, AssessmentWorkflow, input); err != nil {
		s.mu.Lock()
		delete(s.assessments, assessmentID)
		s.mu.Unlock()
		return nil, fmt.Errorf("starting assessment workflow for assessment %s: %w", assessmentID, err)
	}

	if s.fleet != nil {
		s.fleet.OnNewAssessment(assessmentID, 1)
	}

	return info, nil
}

func (s *Service) CancelAssessment(ctx context.Context, assessmentID string) error {
	workflowID := assessmentID
	err := s.temporal.CancelWorkflow(ctx, workflowID, "")
	if err != nil {
		if isWorkflowGone(err) {
			return s.forceStatus(ctx, assessmentID, StatusFailed)
		}
		return err
	}
	return s.UpdateStatus(ctx, assessmentID, StatusFailed)
}

func (s *Service) FinishAssessment(ctx context.Context, assessmentID string) error {
	workflowID := assessmentID
	return s.temporal.SignalWorkflow(ctx, workflowID, "", SignalFinish, true)
}

func isWorkflowGone(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "already completed") ||
		strings.Contains(msg, "already terminated")
}

// forceStatus sets the status unconditionally, bypassing transition
// validation. Used when the workflow is gone and the row is orphaned.
//
// Cache is mutated AFTER the DB write, not before, so a failed write
// doesn't leave the cache and the DB disagreeing.
func (s *Service) forceStatus(ctx context.Context, assessmentID string, status Status) error {
	if s.db == nil {
		s.mu.Lock()
		if info, ok := s.assessments[assessmentID]; ok {
			info.Status = status
			info.UpdatedAt = time.Now()
		}
		s.mu.Unlock()
		return nil
	}
	now := time.Now()
	// Status passed twice: once as the assessment_status enum column,
	// once as text inside to_jsonb. Sharing one parameter trips lib/pq's
	// type inference ("inconsistent types deduced for parameter $1").
	statusStr := status.String()
	_, err := s.db.ExecContext(ctx,
		`UPDATE assessments
		   SET status = $1,
		       updated_at = $2,
		       report = jsonb_set(report, '{status}', to_jsonb($3::text))
		 WHERE id = $4`,
		statusStr, now, statusStr, assessmentID,
	)
	if err != nil {
		return fmt.Errorf("force-setting assessment %s status to %s: %w", assessmentID, status, err)
	}

	s.mu.Lock()
	if info, ok := s.assessments[assessmentID]; ok {
		info.Status = status
		info.UpdatedAt = now
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) GetAssessment(ctx context.Context, assessmentID string) (*Info, error) {
	s.mu.RLock()
	info, ok := s.assessments[assessmentID]
	s.mu.RUnlock()
	if ok {
		return info, nil
	}

	info, err := s.loadAssessment(ctx, assessmentID)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.assessments[assessmentID] = info
	s.mu.Unlock()
	return info, nil
}

type ListFilters struct {
	StatusFilter *Status
	Limit        int
	PageToken    string
}

func (s *Service) ListAssessments(ctx context.Context, filters ListFilters) ([]Info, string, error) {
	limit := filters.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	if s.db == nil {
		s.mu.RLock()
		var results []Info
		for _, info := range s.assessments {
			if filters.StatusFilter != nil && info.Status != *filters.StatusFilter {
				continue
			}
			results = append(results, *info)
		}
		s.mu.RUnlock()
		sort.Slice(results, func(i, j int) bool {
			return results[i].CreatedAt.After(results[j].CreatedAt)
		})
		if len(results) > limit {
			results = results[:limit]
		}
		return results, "", nil
	}

	query := `SELECT id, customer_id, status, config, created_at, updated_at FROM assessments`
	var whereClauses []string
	var args []any
	argIdx := 1

	if filters.StatusFilter != nil {
		whereClauses = append(whereClauses, fmt.Sprintf(`status = $%d`, argIdx))
		args = append(args, filters.StatusFilter.String())
		argIdx++
	}

	if filters.PageToken != "" {
		whereClauses = append(whereClauses, fmt.Sprintf(`created_at < (SELECT created_at FROM assessments WHERE id = $%d)`, argIdx))
		args = append(args, filters.PageToken)
		argIdx++
	}

	if len(whereClauses) > 0 {
		query += ` WHERE ` + strings.Join(whereClauses, ` AND `)
	}

	query += ` ORDER BY created_at DESC`
	query += fmt.Sprintf(` LIMIT $%d`, argIdx)
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("listing assessments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []Info
	for rows.Next() {
		var info Info
		var statusStr string
		var cfgJSON []byte
		if err := rows.Scan(&info.ID, &info.CustomerID, &statusStr, &cfgJSON, &info.CreatedAt, &info.UpdatedAt); err != nil {
			return nil, "", fmt.Errorf("scanning assessment row: %w", err)
		}
		info.Status = StatusFromString(statusStr)
		_ = json.Unmarshal(cfgJSON, &info.Config)
		results = append(results, info)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterating assessment rows: %w", err)
	}

	s.mu.RLock()
	for i := range results {
		if cached, ok := s.assessments[results[i].ID]; ok {
			results[i].Stats = cached.Stats
			results[i].Status = cached.Status
		}
	}
	s.mu.RUnlock()

	// The DB filtered by persisted status, but the cache overlay may
	// have updated a row's status since the last write. Drop rows that
	// no longer match the requested filter.
	if filters.StatusFilter != nil {
		filtered := results[:0]
		for _, r := range results {
			if r.Status == *filters.StatusFilter {
				filtered = append(filtered, r)
			}
		}
		results = filtered
	}

	var nextToken string
	if len(results) > limit {
		results = results[:limit]
		nextToken = results[limit-1].ID
	}

	return results, nextToken, nil
}

func (s *Service) LoadReport(ctx context.Context, assessmentID string) ([]byte, error) {
	if s.db == nil {
		return nil, fmt.Errorf("assessment %s: report not available (no database)", assessmentID)
	}
	var report []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT report FROM assessments WHERE id = $1`, assessmentID,
	).Scan(&report)
	if err != nil {
		return nil, fmt.Errorf("loading report for assessment %s: %w", assessmentID, err)
	}
	if report == nil {
		return nil, fmt.Errorf("assessment %s: no report generated yet", assessmentID)
	}
	return report, nil
}

func (s *Service) UpdateStatus(ctx context.Context, assessmentID string, status Status) error {
	// Validate the transition against the cache, but DO NOT mutate the
	// cache yet. If the DB write fails (e.g. transient pq error) and
	// Temporal retries the activity, a pre-mutated cache would make the
	// retry's ValidateTransition reject the now-same status and return
	// nil silently — leaving the DB stale forever.
	s.mu.RLock()
	info, ok := s.assessments[assessmentID]
	if ok {
		if err := ValidateTransition(info.Status, status); err != nil {
			s.mu.RUnlock()
			return nil
		}
	}
	s.mu.RUnlock()

	if s.db == nil {
		// Cache-only mode (tests): apply the mutation now.
		s.mu.Lock()
		if info, ok := s.assessments[assessmentID]; ok {
			info.Status = status
			info.UpdatedAt = time.Now()
		}
		s.mu.Unlock()
		return nil
	}

	// Update the report blob's status field in the same statement so
	// exports never disagree with the assessment row. jsonb_set on a
	// NULL report (pre-generation) is a no-op, which is what we want
	// during the discovery/exploitation/validation phases.
	//
	// Only update if the row isn't already terminal. Prevents a
	// late-arriving workflow activity from overwriting a cancel.
	//
	// Status passed twice ($1 as enum column, $3 as text into to_jsonb)
	// because lib/pq fails type inference when one $N is both an enum
	// and a text value in the same statement.
	now := time.Now()
	statusStr := status.String()
	_, err := s.db.ExecContext(ctx,
		`UPDATE assessments
		   SET status = $1,
		       updated_at = $2,
		       report = jsonb_set(report, '{status}', to_jsonb($3::text))
		 WHERE id = $4 AND status NOT IN ($5, $6, $7, $8)`,
		statusStr, now, statusStr, assessmentID,
		StatusApproved.String(), StatusRejected.String(), StatusUnreviewed.String(), StatusFailed.String(),
	)
	if err != nil {
		return fmt.Errorf("updating assessment %s status to %s: %w", assessmentID, status, err)
	}

	// DB succeeded — now safe to update the cache.
	s.mu.Lock()
	if info, ok := s.assessments[assessmentID]; ok {
		info.Status = status
		info.UpdatedAt = now
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) UpdateStats(ctx context.Context, assessmentID string, stats Stats) error {
	s.mu.Lock()
	info, ok := s.assessments[assessmentID]
	if ok {
		info.Stats = stats
		info.UpdatedAt = time.Now()
	}
	s.mu.Unlock()

	if s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE assessments SET updated_at = $1 WHERE id = $2`,
		time.Now(), assessmentID,
	)
	if err != nil {
		return fmt.Errorf("updating assessment %s stats: %w", assessmentID, err)
	}
	return nil
}

func (s *Service) persistAssessment(ctx context.Context, info *Info) error {
	if s.db == nil {
		return nil
	}
	cfgJSON, err := json.Marshal(info.Config)
	if err != nil {
		return fmt.Errorf("marshaling assessment config: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO assessments (id, customer_id, status, config, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (id) DO NOTHING`,
		info.ID, info.CustomerID, info.Status.String(), cfgJSON, info.CreatedAt, info.UpdatedAt,
	)
	return err
}

func (s *Service) loadAssessment(ctx context.Context, assessmentID string) (*Info, error) {
	if s.db == nil {
		return nil, fmt.Errorf("assessment %s: %w", assessmentID, ErrAssessmentNotFound)
	}

	var (
		info      Info
		statusStr string
		cfgJSON   []byte
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, customer_id, status, config, created_at, updated_at FROM assessments WHERE id = $1`,
		assessmentID,
	).Scan(&info.ID, &info.CustomerID, &statusStr, &cfgJSON, &info.CreatedAt, &info.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("assessment %s: %w", assessmentID, ErrAssessmentNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("loading assessment %s: %w", assessmentID, err)
	}

	info.Status = StatusFromString(statusStr)
	_ = json.Unmarshal(cfgJSON, &info.Config)
	return &info, nil
}
