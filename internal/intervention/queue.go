package intervention

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/okedeji/agentcage/internal/ids"
)

var ErrNotPending = errors.New("intervention is not in pending status")

type Queue struct {
	store    Store
	notifier Notifier
	logger   logr.Logger
	mu       sync.RWMutex
	pending  map[string]*Request
}

func NewQueue(store Store, notifier Notifier, logger logr.Logger) *Queue {
	return &Queue{
		store:    store,
		notifier: notifier,
		logger:   logger,
		pending:  make(map[string]*Request),
	}
}

func (q *Queue) Enqueue(ctx context.Context, reqType Type, priority Priority, cageID, assessmentID, description string, contextData []byte, timeout time.Duration) (*Request, error) {
	if reqType < TypeTripwireEscalation || reqType > TypePlanApproval {
		return nil, fmt.Errorf("invalid intervention type %d", reqType)
	}
	req := Request{
		ID:           ids.Intervention(),
		Type:         reqType,
		Status:       StatusPending,
		Priority:     priority,
		CageID:       cageID,
		AssessmentID: assessmentID,
		Description:  description,
		ContextData:  contextData,
		Timeout:      timeout,
		CreatedAt:    time.Now(),
	}

	if err := q.store.SaveIntervention(ctx, req); err != nil {
		return nil, fmt.Errorf("saving intervention %s: %w", req.ID, err)
	}

	q.mu.Lock()
	q.pending[req.ID] = &req
	q.mu.Unlock()

	if err := q.notifier.NotifyCreated(ctx, req); err != nil {
		q.logger.Error(err, "notifying intervention created", "intervention_id", req.ID)
	}

	return &req, nil
}

func (q *Queue) Get(ctx context.Context, id string) (*Request, error) {
	req, err := q.store.GetIntervention(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("getting intervention %s: %w", id, err)
	}
	if req == nil {
		return nil, fmt.Errorf("intervention %s not found", id)
	}
	return req, nil
}

func (q *Queue) List(ctx context.Context, filters ListFilters) ([]Request, string, error) {
	items, nextToken, err := q.store.ListInterventions(ctx, filters)
	if err != nil {
		return nil, "", fmt.Errorf("listing interventions: %w", err)
	}
	return items, nextToken, nil
}

func (q *Queue) GetPending(_ context.Context) ([]*Request, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()

	result := make([]*Request, 0, len(q.pending))
	for _, req := range q.pending {
		result = append(result, req)
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority > result[j].Priority
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})

	return result, nil
}

func (q *Queue) Resolve(ctx context.Context, interventionID string, decision Decision) error {
	req, err := q.store.GetIntervention(ctx, interventionID)
	if err != nil {
		return fmt.Errorf("getting intervention %s: %w", interventionID, err)
	}
	if req == nil {
		return fmt.Errorf("intervention %s not found", interventionID)
	}

	if req.Status != StatusPending {
		return fmt.Errorf("intervention %s: %w", interventionID, ErrNotPending)
	}

	now := time.Now()
	req.Status = StatusResolved
	req.ResolvedAt = &now

	if err := q.store.UpdateIntervention(ctx, *req); err != nil {
		return fmt.Errorf("updating intervention %s: %w", interventionID, err)
	}

	q.mu.Lock()
	delete(q.pending, interventionID)
	q.mu.Unlock()

	// Notification failure is logged but not returned. The DB update
	// already succeeded; failing the whole Resolve would leave the
	// caller thinking the intervention is still pending.
	if err := q.notifier.NotifyResolved(ctx, *req); err != nil {
		q.logger.Error(err, "notifying intervention resolved (DB update succeeded, notification lost)",
			"intervention_id", interventionID)
	}

	return nil
}

func (q *Queue) ResolveReview(ctx context.Context, interventionID string, result ReviewResult) error {
	req, err := q.store.GetIntervention(ctx, interventionID)
	if err != nil {
		return fmt.Errorf("getting intervention %s for review: %w", interventionID, err)
	}
	if req == nil {
		return fmt.Errorf("intervention %s not found", interventionID)
	}

	if req.Status != StatusPending {
		return fmt.Errorf("intervention %s: %w", interventionID, ErrNotPending)
	}

	now := time.Now()
	req.Status = StatusResolved
	req.ResolvedAt = &now

	if err := q.store.UpdateIntervention(ctx, *req); err != nil {
		return fmt.Errorf("updating intervention %s for review: %w", interventionID, err)
	}

	q.mu.Lock()
	delete(q.pending, interventionID)
	q.mu.Unlock()

	if err := q.notifier.NotifyResolved(ctx, *req); err != nil {
		q.logger.Error(err, "notifying review resolved (DB update succeeded, notification lost)",
			"intervention_id", interventionID)
	}

	return nil
}

func (q *Queue) TimeOut(ctx context.Context, interventionID string) error {
	req, err := q.store.GetIntervention(ctx, interventionID)
	if err != nil {
		return fmt.Errorf("getting intervention %s for timeout: %w", interventionID, err)
	}
	if req == nil {
		return fmt.Errorf("intervention %s not found", interventionID)
	}

	now := time.Now()
	req.Status = StatusTimedOut
	req.ResolvedAt = &now

	if err := q.store.UpdateIntervention(ctx, *req); err != nil {
		return fmt.Errorf("updating intervention %s for timeout: %w", interventionID, err)
	}

	q.mu.Lock()
	delete(q.pending, interventionID)
	q.mu.Unlock()

	if err := q.notifier.NotifyTimedOut(ctx, *req); err != nil {
		q.logger.Error(err, "notifying intervention timed out (DB update succeeded, notification lost)",
			"intervention_id", interventionID)
	}

	return nil
}

func (q *Queue) GetExpired(now time.Time) []*Request {
	q.mu.RLock()
	defer q.mu.RUnlock()

	var expired []*Request
	for _, req := range q.pending {
		if now.After(req.CreatedAt.Add(req.Timeout)) {
			expired = append(expired, req)
		}
	}
	return expired
}
