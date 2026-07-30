package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSwapHandler_Swaps(t *testing.T) {
	reply := func(body string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body)
		})
	}
	sh := newSwapHandler(reply("first"))

	rec := httptest.NewRecorder()
	sh.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Body.String(); got != "first" {
		t.Fatalf("before swap = %q, want first", got)
	}

	sh.set(reply("second"))
	rec = httptest.NewRecorder()
	sh.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Body.String(); got != "second" {
		t.Fatalf("after swap = %q, want second", got)
	}
}
