package main

import (
	"strings"
	"testing"
)

func TestNonInteractiveCredentials_PasswordStdin(t *testing.T) {
	stdin := strings.NewReader("secret-token\n")
	user, pass, ok, err := nonInteractiveCredentials(stdin, "okedeji", "", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("password-stdin with a username should be fully resolved")
	}
	if user != "okedeji" || pass != "secret-token" {
		t.Errorf("got (%q, %q), want (okedeji, secret-token)", user, pass)
	}
}

func TestNonInteractiveCredentials_PasswordStdinNeedsUsername(t *testing.T) {
	_, _, _, err := nonInteractiveCredentials(strings.NewReader("tok\n"), "", "", true)
	if err == nil {
		t.Fatal("--password-stdin without --username must error")
	}
	if !strings.Contains(err.Error(), "username") {
		t.Errorf("error %q should mention the missing username", err.Error())
	}
}

func TestNonInteractiveCredentials_PasswordStdinConflictsWithFlag(t *testing.T) {
	_, _, _, err := nonInteractiveCredentials(strings.NewReader("tok\n"), "okedeji", "pw", true)
	if err == nil {
		t.Fatal("--password and --password-stdin together must error")
	}
}

func TestNonInteractiveCredentials_BothFlags(t *testing.T) {
	user, pass, ok, err := nonInteractiveCredentials(strings.NewReader(""), "okedeji", "pw", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || user != "okedeji" || pass != "pw" {
		t.Errorf("got (%q, %q, ok=%v), want (okedeji, pw, true)", user, pass, ok)
	}
}

func TestNonInteractiveCredentials_MissingNeedsPrompt(t *testing.T) {
	// No flags, no password-stdin: the command must fall through to an
	// interactive prompt, signalled by ok=false and no error.
	_, _, ok, err := nonInteractiveCredentials(strings.NewReader(""), "", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("missing credentials should signal a prompt is needed (ok=false)")
	}
}
