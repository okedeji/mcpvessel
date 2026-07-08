package reasoner

import (
	"strings"
	"testing"
)

func TestAgentfile_MultipleUses(t *testing.T) {
	got, err := Agentfile(Params{UsesRefs: []string{"@me/github-tools:0.1", "@me/slack-tools:0.1"}, Model: "openai/gpt-4o-mini"})
	if err != nil {
		t.Fatalf("Agentfile: %v", err)
	}
	for _, want := range []string{
		"MODEL openai/gpt-4o-mini",
		"MAIN respond",
		"USES @me/github-tools:0.1",
		"USES @me/slack-tools:0.1",
		"ENTRYPOINT python3 reasoner.py",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Agentfile missing %q; got:\n%s", want, got)
		}
	}
}

func TestAgentfile_DefersModelAndNeedsARef(t *testing.T) {
	got, err := Agentfile(Params{UsesRefs: []string{"@me/x-tools:0.1"}})
	if err != nil {
		t.Fatalf("Agentfile: %v", err)
	}
	if !strings.Contains(got, "MODEL "+deferredModel) {
		t.Errorf("Agentfile did not defer the model; got:\n%s", got)
	}

	if _, err := Agentfile(Params{}); err == nil {
		t.Error("want an error when no tool collection is given to USES")
	}
}
