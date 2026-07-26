package main

import (
	"strings"
	"testing"
)

// The combined trust bundle keeps the system roots and appends the inspect CA,
// each PEM block newline-separated so both remain parseable. This is what lets
// a cage still verify real roots for anything not proxied while trusting the
// proxy's leaf.
func TestBuildTrustBundle(t *testing.T) {
	system := "-----BEGIN CERTIFICATE-----\nSYSTEM\n-----END CERTIFICATE-----" // no trailing newline
	ca := "-----BEGIN CERTIFICATE-----\nINSPECT\n-----END CERTIFICATE-----\n"
	got := buildTrustBundle(system, ca)
	if !strings.Contains(got, "SYSTEM") || !strings.Contains(got, "INSPECT") {
		t.Fatalf("bundle dropped a cert: %q", got)
	}
	if strings.Contains(got, "SYSTEM-----END") || !strings.Contains(got, "-----END CERTIFICATE-----\n-----BEGIN CERTIFICATE-----") {
		t.Errorf("blocks not newline-separated: %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("bundle must end with a newline: %q", got)
	}
}

// A scratch image may carry no system bundle; the inspect CA alone must still
// produce a valid, newline-terminated bundle.
func TestBuildTrustBundle_NoSystemRoots(t *testing.T) {
	ca := "-----BEGIN CERTIFICATE-----\nONLY\n-----END CERTIFICATE-----\n"
	got := buildTrustBundle("", ca)
	if got != ca {
		t.Errorf("with no system roots the bundle should be the CA alone, got %q", got)
	}
}
