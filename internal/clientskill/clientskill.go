// Package clientskill installs the mcpvessel skill into an MCP client, so an
// agent (Claude Code and friends) can drive mcpvessel itself. Each supported
// client has its own skill under skills/<id>/SKILL.md, written for that client's
// tools and register command, so adding a client is one folder plus one registry
// entry.
package clientskill

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed skills
var files embed.FS

// SkillContent returns the embedded skill for a client id, read from
// skills/<id>/SKILL.md.
func SkillContent(clientID string) ([]byte, error) {
	return files.ReadFile("skills/" + clientID + "/SKILL.md")
}

// Client is an MCP client mcpvessel packages a skill for.
type Client struct {
	ID   string // stable id used by --client and the menu, and the skills/<id>/ folder name
	Name string // human label shown in `mcpvessel init`
	// path returns where this client's skill installs on disk.
	path func() (string, error)
}

// Result reports where an install landed.
type Result struct {
	ClientID string
	Path     string
}

// clients are the MCP clients with a packaged skill. Add one by dropping
// skills/<id>/SKILL.md and an entry here.
var clients = []Client{
	{ID: "claude-code", Name: "Claude Code", path: claudeCodeSkillPath},
}

// Clients returns the selectable clients, in menu order.
func Clients() []Client { return clients }

// Path returns where the skill installs for a client id, without writing it.
func Path(clientID string) (string, error) {
	c, err := lookup(clientID)
	if err != nil {
		return "", err
	}
	return c.path()
}

// Install writes the skill packaged in this binary for the given client id.
func Install(clientID string) (Result, error) {
	content, err := SkillContent(clientID)
	if err != nil {
		return Result{}, fmt.Errorf("no skill packaged for %q: %w", clientID, err)
	}
	return InstallContent(clientID, content)
}

// InstallContent writes the given skill content to a client's install location.
// It is how an agent applies an update fetched from the docs MCP, which may be
// newer than the skill baked into this binary, without hardcoding the path.
func InstallContent(clientID string, content []byte) (Result, error) {
	c, err := lookup(clientID)
	if err != nil {
		return Result{}, err
	}
	path, err := c.path()
	if err != nil {
		return Result{}, err
	}
	if err := writeSkill(path, content); err != nil {
		return Result{}, err
	}
	return Result{ClientID: c.ID, Path: path}, nil
}

func lookup(clientID string) (Client, error) {
	for _, c := range clients {
		if c.ID == clientID {
			return c, nil
		}
	}
	return Client{}, fmt.Errorf("no mcpvessel skill for client %q; supported: %s", clientID, clientIDs())
}

func clientIDs() string {
	ids := make([]string, 0, len(clients))
	for _, c := range clients {
		ids = append(ids, c.ID)
	}
	return fmt.Sprint(ids)
}

// claudeCodeSkillPath is the user's personal Claude Code skills location,
// ~/.claude/skills/mcpvessel/SKILL.md, picked up across every project. This is
// the OS home, not VESSEL_HOME.
func claudeCodeSkillPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding your home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "skills", "mcpvessel", "SKILL.md"), nil
}

func writeSkill(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
