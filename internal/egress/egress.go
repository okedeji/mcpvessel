// Package egress is the in-run egress proxy: a hostname-filtering HTTP
// CONNECT proxy that lets a cage reach only the hosts its EGRESS allow:
// policy names. The per-run network is internal, so this proxy is the only
// way out; a cage that declares no allow: never routes through it. It filters
// by the host in the CONNECT line without terminating TLS, so it holds no
// secret and never sees a payload.
package egress

import (
	"io"
	"net"
	"net/http"
	"sync"
)

// Config maps a source (a cage's address on the run network) to the hostnames
// it may reach. A source not in the map, or a host not in its list, is
// refused: default deny.
type Config struct {
	Sources map[string][]string `json:"sources"`
}

// Handler returns an HTTP CONNECT proxy that tunnels to an allowed host and
// refuses everything else. The allow sets are compiled once at boot.
func Handler(cfg Config) http.Handler {
	allow := make(map[string]map[string]bool, len(cfg.Sources))
	for src, hosts := range cfg.Sources {
		set := make(map[string]bool, len(hosts))
		for _, h := range hosts {
			set[h] = true
		}
		allow[src] = set
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "egress proxy only supports CONNECT", http.StatusMethodNotAllowed)
			return
		}
		host := hostOnly(r.Host)
		if !allow[hostOnly(r.RemoteAddr)][host] {
			http.Error(w, "egress to "+host+" not allowed", http.StatusForbidden)
			return
		}
		tunnel(w, r.Host)
	})
}

// tunnel dials the allowed target, tells the client the connection is open,
// and copies bytes both ways until either side closes. It owns the two copy
// goroutines and joins them before returning, so none outlives the request.
func tunnel(w http.ResponseWriter, target string) {
	upstream, err := net.Dial("tcp", target)
	if err != nil {
		http.Error(w, "dialing "+target, http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "egress proxy needs a hijackable connection", http.StatusInternalServerError)
		_ = upstream.Close()
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	var wg sync.WaitGroup
	wg.Add(2)
	go pipe(&wg, upstream, client)
	go pipe(&wg, client, upstream)
	wg.Wait()
	_ = upstream.Close()
	_ = client.Close()
}

func pipe(wg *sync.WaitGroup, dst, src net.Conn) {
	defer wg.Done()
	_, _ = io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}
