package cagefile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestAgent(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cagefile := `runtime python3
deps chromium nmap
pip requests==2.31.0 httpx==0.27.0
entrypoint python3 solver.py
capability discovery
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Cagefile"), []byte(cagefile), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "solver.py"), []byte("print('hello')"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "lib"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib", "utils.py"), []byte("# utils"), 0644))

	return dir
}

func TestPackToFile_ProducesBundle(t *testing.T) {
	agentDir := createTestAgent(t)
	outPath := filepath.Join(t.TempDir(), "test.cage")

	manifest, err := PackToFile(agentDir, "latest", "0.1.0", outPath, 0, nil)
	require.NoError(t, err)
	require.NotNil(t, manifest)

	assert.Equal(t, "python3", manifest.Runtime)
	assert.Equal(t, "python3 solver.py", manifest.Entrypoint)
	assert.Equal(t, []string{"chromium", "nmap"}, manifest.SystemDeps)
	assert.Equal(t, []string{"requests==2.31.0", "httpx==0.27.0"}, manifest.PipDeps)
	assert.True(t, len(manifest.FilesHash) > 10)

	info, err := os.Stat(outPath)
	require.NoError(t, err)
	assert.True(t, info.Size() > 0)
}

func TestPackAndUnpack_RoundTrip(t *testing.T) {
	agentDir := createTestAgent(t)
	bundlePath := filepath.Join(t.TempDir(), "test.cage")

	_, err := PackToFile(agentDir, "latest", "0.1.0", bundlePath, 0, nil)
	require.NoError(t, err)

	extractDir := t.TempDir()
	manifest, err := UnpackFile(bundlePath, extractDir)
	require.NoError(t, err)

	assert.Equal(t, "python3", manifest.Runtime)
	assert.Equal(t, "python3 solver.py", manifest.Entrypoint)
	assert.Equal(t, []string{"chromium", "nmap"}, manifest.SystemDeps)
	assert.Equal(t, []string{"requests==2.31.0", "httpx==0.27.0"}, manifest.PipDeps)

	// Check manifest.json was extracted
	assert.FileExists(t, filepath.Join(extractDir, "manifest.json"))

	// Check agent files were extracted under files/
	assert.FileExists(t, filepath.Join(extractDir, "files", "solver.py"))
	assert.FileExists(t, filepath.Join(extractDir, "files", "lib", "utils.py"))

	// Cagefile should NOT be in the bundle (represented by manifest.json)
	assert.NoFileExists(t, filepath.Join(extractDir, "files", "Cagefile"))

	// Verify content
	content, err := os.ReadFile(filepath.Join(extractDir, "files", "solver.py"))
	require.NoError(t, err)
	assert.Equal(t, "print('hello')", string(content))
}

func TestPack_NoCagefile(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(t.TempDir(), "fail.cage")

	_, err := PackToFile(dir, "latest", "0.1.0", outPath, 0, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Cagefile")
}

func TestPack_InvalidCagefile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Cagefile"), []byte("runtime ruby\nentrypoint ruby x.rb"), 0644))
	outPath := filepath.Join(t.TempDir(), "fail.cage")

	_, err := PackToFile(dir, "latest", "0.1.0", outPath, 0, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported runtime")
}

func TestUnpack_MissingManifest(t *testing.T) {
	// Create a tar.gz without manifest.json
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Cagefile"), []byte("runtime static\nentrypoint ./x\ncapability discovery"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x"), []byte("bin"), 0755))

	bundlePath := filepath.Join(t.TempDir(), "bad.cage")
	_, err := PackToFile(dir, "latest", "0.1.0", bundlePath, 0, nil)
	require.NoError(t, err)

	// The pack always produces a manifest, so this tests the normal path.
	// Unpack of a valid bundle should succeed.
	extractDir := t.TempDir()
	manifest, err := UnpackFile(bundlePath, extractDir)
	require.NoError(t, err)
	assert.Equal(t, "static", manifest.Runtime)
}

func TestPack_StaticRuntime(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Cagefile"), []byte("runtime static\nentrypoint ./scanner\ncapability discovery"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "scanner"), []byte("#!/bin/sh\necho scan"), 0755))

	outPath := filepath.Join(t.TempDir(), "static.cage")
	manifest, err := PackToFile(dir, "latest", "0.1.0", outPath, 0, nil)
	require.NoError(t, err)

	assert.Equal(t, "static", manifest.Runtime)
	assert.Equal(t, "./scanner", manifest.Entrypoint)
	assert.Empty(t, manifest.SystemDeps)
	assert.Empty(t, manifest.PipDeps)
}

func TestPack_FilesHash_ChangesWithContent(t *testing.T) {
	dir := createTestAgent(t)

	out1 := filepath.Join(t.TempDir(), "v1.cage")
	m1, err := PackToFile(dir, "latest", "0.1.0", out1, 0, nil)
	require.NoError(t, err)

	// Modify a file
	require.NoError(t, os.WriteFile(filepath.Join(dir, "solver.py"), []byte("print('changed')"), 0644))

	out2 := filepath.Join(t.TempDir(), "v2.cage")
	m2, err := PackToFile(dir, "latest", "0.1.0", out2, 0, nil)
	require.NoError(t, err)

	assert.NotEqual(t, m1.FilesHash, m2.FilesHash, "hash should change when file content changes")
}
