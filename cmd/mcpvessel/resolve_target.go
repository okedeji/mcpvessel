package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/locate"
	"github.com/okedeji/mcpvessel/internal/progress"
	"github.com/okedeji/mcpvessel/internal/store"
)

// localTarget is a resolved run/call argument: the reference the daemon boots
// (a content hash for a built source directory, else the argument as given),
// the local bundle path to read its manifest, a friendly name for egress and
// secret scoping, and a display string for messages.
type localTarget struct {
	Ref     string
	Path    string
	Name    string
	Display string
}

// resolveLocalTarget resolves a run or call argument. A source directory is
// built into the store here, with build progress on the user's terminal, and
// the resulting content hash is what the daemon boots, so `run ./dir` and
// `call ./dir` work exactly like `serve ./dir` instead of erroring. An
// unchanged directory is a cheap store hit, not a rebuild. Anything else
// (a reference, a content hash, a .agent file) resolves store-first, then
// pulls, via locate.
func resolveLocalTarget(ctx context.Context, stderr io.Writer, arg string) (localTarget, error) {
	if info, err := os.Stat(arg); err == nil && info.IsDir() {
		if _, verr := os.Stat(filepath.Join(arg, bundle.VesselfileName)); verr != nil {
			return localTarget{}, fmt.Errorf("%s is a directory with no %s; point at an agent source directory, a reference, a content hash, or a .agent file", arg, bundle.VesselfileName)
		}
		st, err := store.New()
		if err != nil {
			return localTarget{}, err
		}
		name := filepath.Base(strings.TrimSuffix(arg, string(filepath.Separator)))
		hash, err := bundle.HashSource(arg, st.Dir())
		if err != nil {
			return localTarget{}, err
		}
		path := st.PathFor(hash)
		if _, statErr := os.Stat(path); statErr != nil {
			hash, path, err = buildIntoStore(ctx, stderr, stderr, buildConfig{srcDir: arg, mode: progress.ModeAuto})
			if err != nil {
				return localTarget{}, err
			}
		}
		return localTarget{Ref: hash, Path: path, Name: name, Display: arg}, nil
	}

	b, err := locate.Bundle(ctx, arg)
	if err != nil {
		return localTarget{}, err
	}
	return localTarget{Ref: arg, Path: b.Path, Name: b.Name, Display: b.Display}, nil
}
