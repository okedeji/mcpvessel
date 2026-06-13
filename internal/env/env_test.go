package env

import "testing"

// UsesURL is one half of a cross-language contract: an agent derives the
// same variable name from the same ref on its side (sample/caller reads
// AGENTCAGE_USES_ECHO_URL for `USES @okedeji/echo`). The rule is uppercase
// with dashes turned to underscores; these cases pin it.
func TestUsesURL(t *testing.T) {
	cases := map[string]string{
		"web-search": "AGENTCAGE_USES_WEB_SEARCH_URL",
		"my_agent":   "AGENTCAGE_USES_MY_AGENT_URL",
		"echo":       "AGENTCAGE_USES_ECHO_URL",
	}
	for name, want := range cases {
		if got := UsesURL(name); got != want {
			t.Errorf("UsesURL(%q) = %q, want %q", name, got, want)
		}
	}
}
