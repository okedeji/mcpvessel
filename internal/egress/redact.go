package egress

import (
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
)

// minRedactLen is the shortest granted secret value that is redacted. A very
// short value would match benign text everywhere, so redacting it would blank
// out the request instead of clarifying it. Real secrets are longer.
const minRedactLen = 6

// secretForm pairs one on-the-wire spelling of a granted secret (its raw value
// or a common encoding of it) with the marker that replaces it in a surfaced
// preview.
type secretForm struct {
	from string // the exact bytes to look for
	to   string // the «NAME» marker to substitute
}

// buildSecretForms expands each granted secret into the spellings a server
// might use to ship it, so a naive encoding does not slip a value past the
// preview redaction. It cannot catch a value the server encrypts or otherwise
// transforms before sending; that case is caught by the unexpected-host signal,
// not by content, the same limit a human reader would hit.
func buildSecretForms(secrets []SecretValue) []secretForm {
	var forms []secretForm
	seen := map[string]bool{}
	for _, s := range secrets {
		if len(s.Value) < minRedactLen {
			continue
		}
		marker := "«" + s.Name + "»"
		for _, f := range secretEncodings(s.Value) {
			if len(f) < minRedactLen || seen[f] {
				continue
			}
			seen[f] = true
			forms = append(forms, secretForm{from: f, to: marker})
		}
	}
	// Longest spelling first, so a longer match is replaced before a shorter one
	// that might overlap it (for example a value that is a prefix of another).
	sort.Slice(forms, func(i, j int) bool { return len(forms[i].from) > len(forms[j].from) })
	return forms
}

// secretEncodings returns the raw value and the encodings a server commonly
// applies before putting a secret in a request: base64 (standard and URL, with
// and without padding), hex, and percent-encoding.
func secretEncodings(v string) []string {
	b := []byte(v)
	return []string{
		v,
		base64.StdEncoding.EncodeToString(b),
		base64.RawStdEncoding.EncodeToString(b),
		base64.URLEncoding.EncodeToString(b),
		base64.RawURLEncoding.EncodeToString(b),
		hex.EncodeToString(b),
		url.QueryEscape(v),
	}
}

// previewSecretNames returns the names of granted secrets whose value (in any
// known spelling) appears in the captured request. Names only, for the safe
// "egress secret:" marker; the value never leaves the proxy this way.
func (p *Proxy) previewSecretNames(prev *PreviewRequest) []string {
	if prev == nil || len(p.secretForms) == 0 {
		return nil
	}
	parts := []string{prev.URL, string(prev.Body)}
	for _, vs := range prev.Header {
		parts = append(parts, vs...)
	}
	seen := map[string]bool{}
	var names []string
	for _, f := range p.secretForms {
		name := strings.TrimSuffix(strings.TrimPrefix(f.to, "«"), "»")
		if seen[name] {
			continue
		}
		for _, s := range parts {
			if strings.Contains(s, f.from) {
				seen[name] = true
				names = append(names, name)
				break
			}
		}
	}
	return names
}

// redactString replaces every known secret spelling in s with its marker.
func (p *Proxy) redactString(s string) string {
	for _, f := range p.secretForms {
		if strings.Contains(s, f.from) {
			s = strings.ReplaceAll(s, f.from, f.to)
		}
	}
	return s
}

// redactPreview returns a copy of prev with every granted secret value replaced
// by its «NAME» marker across the URL, the header values, and the body. The
// original is left untouched: the raw request the proxy replays upstream on
// approval is a separate object, so redacting the surfaced copy never changes
// what is actually sent. With no secrets configured it returns prev unchanged.
func (p *Proxy) redactPreview(prev *PreviewRequest) *PreviewRequest {
	if prev == nil || len(p.secretForms) == 0 {
		return prev
	}
	out := *prev
	out.URL = p.redactString(prev.URL)
	if len(prev.Body) > 0 {
		out.Body = []byte(p.redactString(string(prev.Body)))
	}
	if len(prev.Header) > 0 {
		h := make(map[string][]string, len(prev.Header))
		for k, vs := range prev.Header {
			rv := make([]string, len(vs))
			for i, v := range vs {
				rv[i] = p.redactString(v)
			}
			h[k] = rv
		}
		out.Header = h
	}
	return &out
}
