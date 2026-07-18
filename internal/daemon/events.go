package daemon

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Event is one entry on the lifecycle event feed. It carries enough to read
// the feed without a second lookup.
type Event struct {
	Time   time.Time `json:"time"`
	Type   string    `json:"type"`
	RunID  string    `json:"run_id"`
	Ref    string    `json:"ref,omitempty"`
	Target string    `json:"target,omitempty"`
	Status string    `json:"status,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Event types emitted by the daemon itself; other types come from the runtime
// through the run's OnEvent hook.
const (
	EventRunStarted = "run.started"
	EventRunEnded   = "run.ended"
	// EventEgressPending fires when the egress proxy holds a cage's connection to
	// an unapproved host; Target is the host and Detail is the approve command.
	EventEgressPending = "egress.pending"
	// EventEgressApproved fires when a held host is approved and the connection
	// released; Target is the host.
	EventEgressApproved = "egress.approved"
)

// eventBufferSize bounds each subscriber's queue. A watcher this far behind
// loses events rather than blocking the publisher: a stuck events client must
// not stall a run's lifecycle.
const eventBufferSize = 256

// eventBus fans out events to every live subscriber. publish never blocks; a
// subscriber whose buffer is full loses the event.
type eventBus struct {
	mu   sync.Mutex
	next int
	subs map[int]chan Event
}

func newEventBus() *eventBus {
	return &eventBus{subs: map[int]chan Event{}}
}

// subscribe registers a watcher and returns its channel and an unsubscribe to
// defer. unsubscribe closes the channel.
func (b *eventBus) subscribe() (<-chan Event, func()) {
	ch := make(chan Event, eventBufferSize)
	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
		b.mu.Unlock()
	}
}

// publish delivers e to every subscriber, dropping it for any whose buffer is
// full. Send and close are both under the lock and unsubscribe deletes from
// the map before closing, so publish never sends on a closed channel.
func (b *eventBus) publish(e Event) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// handleEvents streams the event feed to a client until it disconnects.
func (d *Daemon) handleEvents(w http.ResponseWriter, r *http.Request) {
	ch, unsub := d.events.subscribe()
	defer unsub()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			if err := enc.Encode(e); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}
