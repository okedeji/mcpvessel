package runtime

// Event is a runtime lifecycle notification for the daemon's events feed. The
// runtime owns the type (the daemon depends on the runtime, not the reverse)
// and stamps the run id. The observer is nil for every caller but the daemon.
type Event struct {
	RunID  string
	Type   string
	Target string // the sub-agent node the event concerns; empty when run-wide
	Detail string
}

// In-process runtime events. Gateway-container events (per LLM or sub-agent
// call) reach the daemon over the collector channel instead.
const (
	EventCageActivated       = "cage.activated"
	EventCageEvicted         = "cage.evicted"
	EventElicitationAsked    = "elicitation.asked"
	EventElicitationAnswered = "elicitation.answered"
)

func emitEvent(onEvent func(Event), e Event) {
	if onEvent != nil {
		onEvent(e)
	}
}

func (w *workingSet) event(typ, target, detail string) {
	emitEvent(w.onEvent, Event{RunID: w.runID, Type: typ, Target: target, Detail: detail})
}
