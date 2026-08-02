package daemon

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/serve"
)

// mcpvessel ships its own docs as a caged MCP server so a driving agent can look
// up how the tool behaves instead of guessing. init opts the operator in; the
// daemon then serves it on a fixed loopback port the client is registered
// against, so that URL keeps working across daemon restarts.
const (
	// DocsRef is the published bundle reference for the mcpvessel-docs server.
	DocsRef = "io.github.okedeji/mcpvessel-docs"
	// DocsListen is the fixed loopback address the docs front door binds. Fixed
	// so the URL registered with the client survives daemon restarts.
	DocsListen = "127.0.0.1:7333"
)

// DocsURL is the merged endpoint an MCP client registers to reach the caged
// docs server.
func DocsURL() string {
	return "http://" + DocsListen + serve.FlatPath
}

// ensureDocsServed brings the caged docs server up on DocsListen, or returns nil
// if it is already there. Safe to call on every startup: docsMu keeps it from
// racing an init-triggered ensure on the port, and the frontByListen check skips
// the work when the door is already open. The pull can fail (docs not published,
// no network); that error is the caller's to log, and the rest of the daemon
// runs regardless.
func (d *Daemon) ensureDocsServed(ctx context.Context) error {
	d.docsMu.Lock()
	defer d.docsMu.Unlock()

	if d.frontByListen(DocsListen) != nil {
		return nil
	}
	// No secrets, no operator egress: the docs bundle bakes its own allow-list
	// (the GitHub hosts it reads), so it works with nothing granted. An optional
	// GitHub token only lifts rate limits; the operator can add it later, init
	// does not ask.
	req := serveRequest{
		Bundles: []serveBundle{{Ref: DocsRef}},
		Listen:  DocsListen,
	}
	if _, _, _, err := d.openServe(ctx, req); err != nil {
		return fmt.Errorf("serving %s: %w", DocsRef, err)
	}
	return nil
}

// ensureDocsResult is the POST /docs/ensure response: the URL to register with
// the MCP client, and whether the door was already open.
type ensureDocsResult struct {
	URL            string `json:"url"`
	AlreadyServing bool   `json:"already_serving"`
}

// handleEnsureDocs serves the docs server now and persists the opt-in, so init
// gets docs running without a daemon restart and later startups re-serve it.
func (d *Daemon) handleEnsureDocs(w http.ResponseWriter, r *http.Request) {
	already := d.frontByListen(DocsListen) != nil

	// Persist the opt-in before serving, not after. Reaching this handler means
	// the operator chose the docs server; whether the pull happens to succeed
	// right now is a separate question. Persisting only on success made one
	// stalled registry read during first-run setup permanent: the flag stayed
	// off, so every later startup skipped docs too, silently, and nothing told
	// the operator their choice had been dropped. A startup that cannot reach
	// the registry warns and carries on, which is the lesser cost.
	if cfg, err := config.Load(); err == nil && !cfg.Docs.Enabled {
		cfg.Docs.Enabled = true
		if serr := cfg.Save(); serr != nil {
			fmt.Fprintf(os.Stderr, "warning: persisting docs opt-in: %v\n", serr)
		}
	}

	if err := d.ensureDocsServed(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, ensureDocsResult{URL: DocsURL(), AlreadyServing: already})
}
