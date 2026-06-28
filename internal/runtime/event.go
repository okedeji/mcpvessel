package runtime

// Event is a runtime lifecycle notification the daemon surfaces on its
// `agentcage events` feed: a sub-agent cage activating on demand or being
// reaped, a question asked of the operator and its answer. The runtime owns the
// type (the daemon depends on the runtime, not the reverse) and stamps the run
// id, so the daemon publishes one without knowing the run's shape. The observer
// is nil for every caller but the daemon, which pays nothing.
type Event struct {
	RunID  string
	Type   string
	Target string // the sub-agent node the event concerns; empty when run-wide
	Detail string
}

// Runtime event types: events the runtime produces in
// process. The gateway-container events (per LLM call, per sub-agent call) reach
// the daemon over the collector channel instead.
const (
	EventCageActivated       = "cage.activated"
	EventCageEvicted         = "cage.evicted"
	EventElicitationAsked    = "elicitation.asked"
	EventElicitationAnswered = "elicitation.answered"
)

// emitEvent delivers e to the observer when one is set.
func emitEvent(onEvent func(Event), e Event) {
	if onEvent != nil {
		onEvent(e)
	}
}

// event reports a working-set event stamped with the run id.
func (w *workingSet) event(typ, target, detail string) {
	emitEvent(w.onEvent, Event{RunID: w.runID, Type: typ, Target: target, Detail: detail})
}
