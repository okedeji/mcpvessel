package secrets

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/env"
)

func TestLoad_MissingIsEmpty(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Names()) != 0 {
		t.Errorf("missing store should be empty, got %v", s.Names())
	}
}

func TestSaveLoad_RoundTrips(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	s, _ := Load()
	s.Set("openai_key", "sk-secret-value")
	s.Set("notion_token", "ntn-abc")
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if v, ok := got.Get("openai_key"); !ok || v != "sk-secret-value" {
		t.Errorf("Get(openai_key) = %q, %v", v, ok)
	}
	if !reflect.DeepEqual(got.Names(), []string{"notion_token", "openai_key"}) {
		t.Errorf("Names = %v, want sorted", got.Names())
	}
	if !got.Remove("openai_key") || got.Remove("missing") {
		t.Error("Remove behavior wrong")
	}
}

func TestStore_Redacts(t *testing.T) {
	s := &Store{}
	s.Set("k", "super-secret")

	if got := fmt.Sprintf("%v %#v", *s, *s); strings.Contains(got, "super-secret") {
		t.Errorf("String/GoString leaked the value: %q", got)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "super-secret") {
		t.Errorf("MarshalJSON leaked the value: %s", raw)
	}
}
