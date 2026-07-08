package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/okedeji/agentcage/internal/mcp"
)

func okTarget(_ context.Context, q *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"q": q.Message}}, nil
}

func TestElicitRouter_RoutesToBoundTarget(t *testing.T) {
	r := newElicitRouter()
	release := r.bind(okTarget)
	defer release()

	res, err := r.route(context.Background(), &mcp.ElicitRequest{Message: "hi"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if res.Content["q"] != "hi" {
		t.Errorf("answer = %v, want the question echoed", res.Content["q"])
	}
}

func TestElicitRouter_EmitsAskedThenAnswered(t *testing.T) {
	r := newElicitRouter()
	r.runID = "run-1"
	var got []Event
	r.onEvent = func(e Event) { got = append(got, e) }

	release := r.bind(okTarget)
	defer release()
	if _, err := r.route(context.Background(), &mcp.ElicitRequest{Message: "hi"}); err != nil {
		t.Fatalf("route: %v", err)
	}

	if len(got) != 2 || got[0].Type != EventElicitationAsked || got[1].Type != EventElicitationAnswered {
		t.Fatalf("events = %+v, want asked then answered", got)
	}
	if got[0].RunID != "run-1" {
		t.Errorf("asked not stamped with run id: %+v", got[0])
	}
	if got[1].Detail != "accept" {
		t.Errorf("answered detail = %q, want the action", got[1].Detail)
	}
}

func TestElicitRouter_NoTargetEmitsNothing(t *testing.T) {
	r := newElicitRouter()
	fired := false
	r.onEvent = func(Event) { fired = true }
	_, _ = r.route(context.Background(), &mcp.ElicitRequest{Message: "x"})
	if fired {
		t.Error("a question with no caller must not emit an asked/answered event")
	}
}

func TestElicitRouter_NoTargetErrors(t *testing.T) {
	r := newElicitRouter()
	if _, err := r.route(context.Background(), &mcp.ElicitRequest{Message: "x"}); err == nil {
		t.Fatal("want an error with nothing bound")
	}

	// A released bind leaves nothing live, so it errors again.
	r.bind(okTarget)()
	if _, err := r.route(context.Background(), &mcp.ElicitRequest{Message: "x"}); err == nil {
		t.Fatal("want an error after the bind released")
	}
}

func TestElicitRouter_SerializesCalls(t *testing.T) {
	r := newElicitRouter()
	release := r.bind(okTarget)

	done := make(chan struct{})
	go func() {
		r.bind(okTarget)()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("second bind returned while the first still held")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second bind never returned after release")
	}
}

func TestElicitRouter_RespectsContextCancel(t *testing.T) {
	r := newElicitRouter()
	release := r.bind(func(ctx context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := r.route(ctx, &mcp.ElicitRequest{Message: "x"}); err == nil {
		t.Fatal("want an error when the caller context is cancelled")
	}
}
