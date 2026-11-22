package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "siteinfo ") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestServeFailsOnInvalidConfig(t *testing.T) {
	t.Setenv("SITEINFO_AUTH_TOKENS", "")
	cmd := newRootCmd()
	cmd.SetArgs([]string{"serve"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "auth.tokens") {
		t.Fatalf("err = %v, want a validation error mentioning auth.tokens", err)
	}
}
