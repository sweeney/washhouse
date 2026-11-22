package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionFlagPrintsVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"-version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "washhouse "+version {
		t.Errorf("got %q", got)
	}
}

// Until the service exists, running it must fail loudly rather than look
// like a service that started and published nothing.
func TestWithoutFlagsItRefusesToRun(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "not implemented") {
		t.Errorf("stderr %q", errOut.String())
	}
}

func TestUnknownFlagIsAnError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"-nope"}, &out, &errOut); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
}
