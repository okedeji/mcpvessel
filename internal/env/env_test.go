package env

import "testing"

// UsesURL is one half of a cross-language contract: the SDK's _env_var_for
// derives the same name on the agent side. These cases mirror the SDK's
// own test (sdk/python/tests/test_agents.py), so the two stay in lockstep.
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
