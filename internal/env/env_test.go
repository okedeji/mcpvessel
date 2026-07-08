package env

import "testing"

// Pins the cross-language naming rule; the agent side derives the same name.
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
