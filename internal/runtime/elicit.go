package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/okedeji/agentcage/internal/mcp"
)

// elicitDeadline bounds a mid-call question's wait for the operator. A human
// answers, so minutes, not seconds; past it the elicit errors and the cage is
// freed rather than pinned forever on an unanswered question.
const elicitDeadline = 3 * time.Minute

// elicitRouter carries one served run's mid-call questions to whoever is
// driving the current call. MCP's elicitation channel carries no correlation
// back to the triggering call, so a run answers one eliciting call at a time:
// bind blocks until the previous call releases. That serializes calls to a
// served interactive agent; accepted.
type elicitRouter struct {
	call   sync.Mutex // held for a call's duration; serializes eliciting calls
	mu     sync.Mutex // guards target
	target mcp.ElicitHandler

	// onEvent and runID feed the daemon's event feed. route is the single
	// choke point, so asked/answered report once per elicitation regardless
	// of depth. Nil off the daemon path.
	onEvent func(Event)
	runID   string
}

func newElicitRouter() *elicitRouter { return &elicitRouter{} }

// bind installs target as the run's current answer channel and returns a
// release. It blocks until any in-flight call releases; only one call's
// target is ever live.
func (r *elicitRouter) bind(target mcp.ElicitHandler) func() {
	r.call.Lock()
	r.mu.Lock()
	r.target = target
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		r.target = nil
		r.mu.Unlock()
		r.call.Unlock()
	}
}

// route delivers one question to the bound caller, bounded by elicitDeadline.
// Nothing bound errors, failing the call closed.
func (r *elicitRouter) route(ctx context.Context, q *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	r.mu.Lock()
	target := r.target
	r.mu.Unlock()
	if target == nil {
		return nil, fmt.Errorf("no caller is available to answer this question")
	}
	emitEvent(r.onEvent, Event{RunID: r.runID, Type: EventElicitationAsked})
	ctx, cancel := context.WithTimeout(ctx, elicitDeadline)
	defer cancel()
	res, err := target(ctx, q)
	if err == nil && res != nil {
		emitEvent(r.onEvent, Event{RunID: r.runID, Type: EventElicitationAnswered, Detail: res.Action})
	}
	return res, err
}
