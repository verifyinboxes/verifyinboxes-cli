//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// TestInstallScriptInterrupted: Ctrl-C while the binary is being put in place leaves nothing half-made behind.
func TestInstallScriptInterrupted(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install.sh is for Linux and macOS")
	}
	for _, tool := range []string{"sh", "curl", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s on the PATH", tool)
		}
	}
	base, bin := t.TempDir(), filepath.Join(t.TempDir(), "bin")
	release(t, base, "0.0.3-test", false)
	// The chmod of the copied binary is when Ctrl-C comes: SIGINT to the whole process group, as a terminal sends it.
	shims := t.TempDir()
	if err := os.WriteFile(filepath.Join(shims, "chmod"), []byte("#!/bin/sh\nkill -INT 0\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", script)
	cmd.Env = append(os.Environ(), "PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"),
		"VERIFYINBOXES_DOWNLOAD_BASE=file://"+filepath.ToSlash(base), "VERIFYINBOXES_VERSION=0.0.3-test",
		"VERIFYINBOXES_INSTALL_DIR="+bin, "HOME="+t.TempDir())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // its own group: the test is not interrupted
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("install.sh went on after Ctrl-C:\n%s", out)
	}
	entries, err := os.ReadDir(bin)
	if err != nil {
		t.Fatalf("install.sh did not get as far as the install directory: %v\n%s", err, out)
	}
	for _, entry := range entries {
		t.Errorf("left in %s after Ctrl-C: %s", bin, entry.Name())
	}
}
