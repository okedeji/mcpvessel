package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/okedeji/agentcage/internal/mcp"
)

// elicitDeadline bounds how long an agent's mid-call question waits for the
// operator before the call fails closed. A human is on the other end, so this
// is minutes, not seconds. Past it the elicit errors, the tool call returns
// that error, and the cage is freed rather than pinned forever on a question
// nobody is going to answer.
const elicitDeadline = 3 * time.Minute

// elicitRouter carries one served run's mid-call questions from the agent to
// whoever is driving the current call. The agent's root MCP client routes a
// question in through route; the serve front door binds the operator's answer
// channel for the duration of each call through bind.
//
// MCP's elicitation channel carries no correlation back to the call that
// triggered it, so a run answers one eliciting call at a time: bind blocks
// until the previous call releases. That keeps an answer going to the caller
// that asked rather than to whoever happened to bind last, at the cost of
// serializing calls to a served interactive agent. We accept that.
type elicitRouter struct {
	call   sync.Mutex // held for a call's duration; serializes eliciting calls
	mu     sync.Mutex // guards target
	target mcp.ElicitHandler

	// onEvent and runID feed the daemon's live event feed. route is the single
	// choke point for every question, root or sub-agent, so the asked/answered
	// pair reports here once per elicitation regardless of depth. Nil off the
	// daemon path.
	onEvent func(Event)
	runID   string
}

func newElicitRouter() *elicitRouter { return &elicitRouter{} }

// bind installs target as the run's current answer channel and returns a
// release. It blocks until any in-flight call releases, so only one call's
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
// With nothing bound, no call in flight or a path with no live operator, it
// errors, which surfaces to the agent as a failed elicit and fails the call
// closed.
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
