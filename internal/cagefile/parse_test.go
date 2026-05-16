package cagefile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_FullCagefile(t *testing.T) {
	input := `
# XBOW solver agent
runtime python3
deps chromium nmap sqlmap interactsh
pip requests==2.31.0 playwright==1.40.0 httpx==0.27.0 beautifulsoup4==4.12.2
entrypoint python3 solver.py
capability discovery
`
	m, err := ParseString(input)
	require.NoError(t, err)

	assert.Equal(t, "python3", m.Runtime)
	assert.Equal(t, "python3 solver.py", m.Entrypoint)
	assert.Equal(t, []string{"chromium", "nmap", "sqlmap", "interactsh"}, m.SystemDeps)
	assert.Equal(t, []string{"requests==2.31.0", "playwright==1.40.0", "httpx==0.27.0", "beautifulsoup4==4.12.2"}, m.PipDeps)
}

func TestParse_MinimalCagefile(t *testing.T) {
	input := `
runtime static
entrypoint ./my-scanner
capability discovery
`
	m, err := ParseString(input)
	require.NoError(t, err)

	assert.Equal(t, "static", m.Runtime)
	assert.Equal(t, "./my-scanner", m.Entrypoint)
	assert.Empty(t, m.SystemDeps)
	assert.Empty(t, m.PipDeps)
}

func TestParse_NodeRuntime(t *testing.T) {
	input := `
runtime node
npm puppeteer@21.0.0 axios@1.6.0
deps chromium
entrypoint node index.js
capability discovery
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
deps nmap
entrypoint ./scanner
capability discovery
`
	m, err := ParseString(input)
	require.NoError(t, err)

	assert.Equal(t, "go", m.Runtime)
	assert.Equal(t, []string{"github.com/projectdiscovery/nuclei/v3@v3.1.0"}, m.GoDeps)
}

func TestParse_CommentsAndBlankLines(t *testing.T) {
	input := `
# This is a comment

runtime python3

# Another comment
entrypoint python3 solver.py
capability discovery

`
	m, err := ParseString(input)
	require.NoError(t, err)
	assert.Equal(t, "python3", m.Runtime)
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
capability discovery
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
capability discovery
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
capability discovery
`
	_, err := ParseString(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate runtime")
}

func TestParse_DuplicateEntrypoint(t *testing.T) {
	input := `
runtime python3
entrypoint python3 a.py
capability discovery
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
capability discovery
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

func TestParse_PipWithNodeRuntime(t *testing.T) {
	input := `
runtime node
pip requests==2.31.0
entrypoint node index.js
capability discovery
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
capability discovery
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
capability discovery
`
	_, err := ParseString(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "language dependencies are not valid for static runtime")
}

func TestParse_MultipleDepsLines(t *testing.T) {
	input := `
runtime python3
deps chromium nmap
deps sqlmap curl
pip requests==2.31.0
pip httpx==0.27.0 playwright==1.40.0
entrypoint python3 solver.py
capability discovery
`
	m, err := ParseString(input)
	require.NoError(t, err)
	assert.Equal(t, []string{"chromium", "nmap", "sqlmap", "curl"}, m.SystemDeps)
	assert.Equal(t, []string{"requests==2.31.0", "httpx==0.27.0", "playwright==1.40.0"}, m.PipDeps)
}
