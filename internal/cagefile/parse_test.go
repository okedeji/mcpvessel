package cagefile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_FullCagefile(t *testing.T) {
	input := `
# XBOW solver agent
RUNTIME python3
DEPS chromium httpx sqlmap interactsh
PIP requests==2.31.0 playwright==1.40.0 httpx==0.27.0 beautifulsoup4==4.12.2
ENTRYPOINT python3 solver.py

DISCOVERY
EXPLOITATION sqli xss
`
	m, err := ParseString(input)
	require.NoError(t, err)

	assert.Equal(t, "python3", m.Runtime)
	assert.Equal(t, "python3 solver.py", m.Entrypoint)
	assert.Equal(t, []string{"chromium", "httpx", "sqlmap", "interactsh"}, m.SystemDeps)
	assert.Equal(t, []string{"requests==2.31.0", "playwright==1.40.0", "httpx==0.27.0", "beautifulsoup4==4.12.2"}, m.PipDeps)
	assert.True(t, m.Capabilities.Discovery)
	assert.Equal(t, []string{"sqli", "xss"}, m.Capabilities.Exploitation)
}

func TestParse_MinimalCagefile(t *testing.T) {
	input := `
runtime static
entrypoint ./my-scanner
discovery
`
	m, err := ParseString(input)
	require.NoError(t, err)

	assert.Equal(t, "static", m.Runtime)
	assert.Equal(t, "./my-scanner", m.Entrypoint)
	assert.True(t, m.Capabilities.Discovery)
	assert.Empty(t, m.SystemDeps)
	assert.Empty(t, m.PipDeps)
}

func TestParse_NodeRuntime(t *testing.T) {
	input := `
runtime node
npm puppeteer@21.0.0 axios@1.6.0
deps chromium
entrypoint node index.js
discovery
`
	m, err := ParseString(input)
	require.NoError(t, err)

	assert.Equal(t, "node", m.Runtime)
	assert.Equal(t, []string{"puppeteer@21.0.0", "axios@1.6.0"}, m.NpmDeps)
	assert.Equal(t, []string{"chromium"}, m.SystemDeps)
}

func TestParse_GoRuntime(t *testing.T) {
	input := `
runtime go
go-deps github.com/projectdiscovery/nuclei/v3@v3.1.0
deps httpx
entrypoint ./scanner
discovery
`
	m, err := ParseString(input)
	require.NoError(t, err)

	assert.Equal(t, "go", m.Runtime)
	assert.Equal(t, []string{"github.com/projectdiscovery/nuclei/v3@v3.1.0"}, m.GoDeps)
}

func TestParse_AllCapabilities(t *testing.T) {
	input := `
runtime node
entrypoint node agent.js
discovery
exploitation sqli xss info_disclosure
validation
`
	m, err := ParseString(input)
	require.NoError(t, err)

	assert.True(t, m.Capabilities.Discovery)
	assert.Equal(t, []string{"sqli", "xss", "info_disclosure"}, m.Capabilities.Exploitation)
	assert.True(t, m.Capabilities.Validation)
}

func TestParse_CommentsAndBlankLines(t *testing.T) {
	input := `
# This is a comment

runtime python3

# Another comment
entrypoint python3 solver.py
discovery

`
	m, err := ParseString(input)
	require.NoError(t, err)
	assert.Equal(t, "python3", m.Runtime)
	assert.True(t, m.Capabilities.Discovery)
}

func TestParse_UppercaseDirectives(t *testing.T) {
	input := `
RUNTIME python3
ENTRYPOINT python3 solver.py
DISCOVERY
`
	m, err := ParseString(input)
	require.NoError(t, err)
	assert.Equal(t, "python3", m.Runtime)
	assert.True(t, m.Capabilities.Discovery)
}

func TestParse_MissingRuntime(t *testing.T) {
	input := `entrypoint python3 solver.py`
	_, err := ParseString(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runtime is required")
}

func TestParse_MissingEntrypoint(t *testing.T) {
	input := `runtime python3`
	_, err := ParseString(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "entrypoint is required")
}

func TestParse_UnsupportedRuntime(t *testing.T) {
	input := `
runtime ruby
entrypoint ruby scan.rb
discovery
`
	_, err := ParseString(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported runtime")
}

func TestParse_UnsupportedDep(t *testing.T) {
	input := `
runtime python3
deps metasploit
entrypoint python3 solver.py
discovery
`
	_, err := ParseString(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported system dependency")
}

func TestParse_DuplicateRuntime(t *testing.T) {
	input := `
runtime python3
runtime node
entrypoint python3 solver.py
discovery
`
	_, err := ParseString(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate runtime")
}

func TestParse_DuplicateEntrypoint(t *testing.T) {
	input := `
runtime python3
entrypoint python3 a.py
discovery
entrypoint python3 b.py
`
	_, err := ParseString(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate entrypoint")
}

func TestParse_UnknownDirective(t *testing.T) {
	input := `
runtime python3
foobar baz
entrypoint python3 solver.py
discovery
`
	_, err := ParseString(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown directive")
}

func TestParse_DirectiveWithoutValue(t *testing.T) {
	input := `runtime`
	_, err := ParseString(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a value")
}

func TestParse_ExploitationWithoutTools(t *testing.T) {
	input := `
runtime node
entrypoint node agent.js
exploitation
`
	_, err := ParseString(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"exploitation" requires a value`)
}

func TestParse_PipWithNodeRuntime(t *testing.T) {
	input := `
runtime node
pip requests==2.31.0
entrypoint node index.js
discovery
`
	_, err := ParseString(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pip dependencies are not valid for node runtime")
}

func TestParse_NpmWithPythonRuntime(t *testing.T) {
	input := `
runtime python3
npm puppeteer@21.0.0
entrypoint python3 solver.py
discovery
`
	_, err := ParseString(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "npm dependencies are not valid for python3 runtime")
}

func TestParse_LanguageDepsWithStaticRuntime(t *testing.T) {
	input := `
runtime static
pip requests==2.31.0
entrypoint ./scanner
discovery
`
	_, err := ParseString(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "language dependencies are not valid for static runtime")
}

func TestParse_MultipleDepsLines(t *testing.T) {
	input := `
runtime python3
deps chromium httpx
deps sqlmap curl
pip requests==2.31.0
pip httpx==0.27.0 playwright==1.40.0
entrypoint python3 solver.py
discovery
`
	m, err := ParseString(input)
	require.NoError(t, err)
	assert.Equal(t, []string{"chromium", "httpx", "sqlmap", "curl"}, m.SystemDeps)
	assert.Equal(t, []string{"requests==2.31.0", "httpx==0.27.0", "playwright==1.40.0"}, m.PipDeps)
}
