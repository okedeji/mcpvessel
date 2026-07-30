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

	// Serve first, persist only on success. If the bundle is not published or the
	// pull fails, leave the flag off so the daemon does not retry-and-warn every
	// startup; re-running init once docs is reachable makes it stick.
	if err := d.ensureDocsServed(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	// A save failure is not fatal: docs is already serving this session, the
	// operator just loses the auto-reserve on the next startup.
	if cfg, err := config.Load(); err == nil && !cfg.Docs.Enabled {
		cfg.Docs.Enabled = true
		if serr := cfg.Save(); serr != nil {
			fmt.Fprintf(os.Stderr, "warning: persisting docs opt-in: %v\n", serr)
		}
	}

	writeJSON(w, http.StatusOK, ensureDocsResult{URL: DocsURL(), AlreadyServing: already})
}
