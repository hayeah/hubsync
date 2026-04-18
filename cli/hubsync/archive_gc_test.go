package main

import (
	"strings"
	"testing"
)

func TestCmdArchiveGC_MissingHubsyncDir_Exits2(t *testing.T) {
	dir := t.TempDir()
	_, stderr, code := runCLI(t, dir, nil, "archive-gc", ".")
	if code != 2 {
		t.Fatalf("code=%d want 2; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "no .hubsync/") {
		t.Errorf("stderr should explain missing .hubsync; got: %s", stderr)
	}
}

func TestCmdArchiveGC_MissingArchiveConfig_Exits2(t *testing.T) {
	dir := t.TempDir()
	seedHub(t, dir, false)
	_, stderr, code := runCLI(t, dir, nil, "archive-gc", ".")
	if code != 2 {
		t.Fatalf("code=%d want 2; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "archive not configured") {
		t.Errorf("stderr should mention missing [archive] section; got: %s", stderr)
	}
}

func TestCmdArchiveGC_TooManyArgs_Exits2(t *testing.T) {
	_, stderr, code := runCLI(t, "", nil, "archive-gc", "a", "b")
	if code != 2 {
		t.Fatalf("code=%d want 2; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "usage: hubsync archive-gc") {
		t.Errorf("stderr lacks usage line: %s", stderr)
	}
}
