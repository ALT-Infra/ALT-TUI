//go:build linux && amd64

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedBubblewrapInstallsVerifiedExecutable(t *testing.T) {
	directory := t.TempDir()
	path, err := installEmbeddedBubblewrap(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	if filepath.Dir(path) != directory {
		t.Fatalf("path = %q, outside private directory", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %o, want 700", info.Mode().Perm())
	}
	output, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("run embedded Bubblewrap: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "bubblewrap 0.12.0") {
		t.Fatalf("version = %q", output)
	}
	output, err = exec.Command(
		path,
		"--die-with-parent",
		"--unshare-user",
		"--unshare-pid",
		"--unshare-net",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--",
		"/bin/sh", "-lc", "printf isolated",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("use embedded Bubblewrap: %v\n%s", err, output)
	}
	if string(output) != "isolated" {
		t.Fatalf("sandbox output = %q", output)
	}
}
