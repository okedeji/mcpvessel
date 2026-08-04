package egress

import "testing"

// One proxy serves every cage in a run, so two cages can have a pending request
// for the same host at once. Keyed by host alone, the second overwrote the
// first, and the operator could weigh one cage's payload while approving the
// host for another.
func TestPreview_TwoCagesOnOneHostDoNotOverwriteEachOther(t *testing.T) {
	p := New(Config{Sources: map[string][]string{"10.0.0.1": nil, "10.0.0.2": nil}}, nil)

	first := &PreviewRequest{Method: "GET", URL: "/harmless"}
	second := &PreviewRequest{Method: "POST", URL: "/collect", Body: []byte("secret")}
	p.storePreview("10.0.0.1", "api.example.com", first)
	p.storePreview("10.0.0.2", "api.example.com", second)

	if got := p.getPreview("10.0.0.1", "api.example.com"); got != first {
		t.Fatalf("first cage's preview = %+v, want its own request back", got)
	}
	if got := p.getPreview("10.0.0.2", "api.example.com"); got != second {
		t.Fatalf("second cage's preview = %+v, want its own request back", got)
	}
}

// Clearing one cage's preview must leave a sibling's pending attempt on the
// same host alone; that cage is still waiting on its own decision.
func TestPreview_ClearingOneCageLeavesTheOther(t *testing.T) {
	p := New(Config{Sources: map[string][]string{"10.0.0.1": nil, "10.0.0.2": nil}}, nil)
	kept := &PreviewRequest{Method: "GET", URL: "/still-pending"}
	p.storePreview("10.0.0.1", "api.example.com", &PreviewRequest{Method: "GET", URL: "/done"})
	p.storePreview("10.0.0.2", "api.example.com", kept)

	p.clearPreview("10.0.0.1", "api.example.com")

	if got := p.getPreview("10.0.0.1", "api.example.com"); got != nil {
		t.Fatalf("cleared preview came back: %+v", got)
	}
	if got := p.getPreview("10.0.0.2", "api.example.com"); got != kept {
		t.Fatalf("sibling cage's preview = %+v, want it untouched", got)
	}
}

// The control path can arrive without a source (it carries only a host), and a
// single-cage run is the common case, so a source-less pull still answers.
func TestPreview_SourcelessPullStillFindsIt(t *testing.T) {
	p := New(Config{Sources: map[string][]string{"10.0.0.1": nil}}, nil)
	want := &PreviewRequest{Method: "POST", URL: "/collect"}
	p.storePreview("10.0.0.1", "api.example.com", want)

	if got := p.getPreview("", "api.example.com"); got != want {
		t.Fatalf("source-less pull = %+v, want the pending request", got)
	}
	if got := p.getPreview("", "other.example.com"); got != nil {
		t.Fatalf("source-less pull invented a preview for another host: %+v", got)
	}
}

// A decision that names no source resolves the host for every cage that asked,
// so it must drop all of their previews rather than one arbitrary entry.
func TestPreview_SourcelessDecisionClearsEveryCage(t *testing.T) {
	p := New(Config{Sources: map[string][]string{"10.0.0.1": nil, "10.0.0.2": nil}}, nil)
	p.storePreview("10.0.0.1", "api.example.com", &PreviewRequest{Method: "GET"})
	p.storePreview("10.0.0.2", "api.example.com", &PreviewRequest{Method: "POST"})
	p.storePreview("10.0.0.1", "other.example.com", &PreviewRequest{Method: "GET"})

	p.decide("", "api.example.com", true, false)

	for _, src := range []string{"10.0.0.1", "10.0.0.2"} {
		if got := p.getPreview(src, "api.example.com"); got != nil {
			t.Fatalf("%s still has a preview for the decided host: %+v", src, got)
		}
	}
	// A decision about one host says nothing about another.
	if got := p.getPreview("10.0.0.1", "other.example.com"); got == nil {
		t.Fatal("deciding one host cleared a pending preview for a different host")
	}
}
