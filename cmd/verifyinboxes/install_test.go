package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// release makes what a release holds for this machine in dir: the archive and checksums.txt (with a wrong checksum
// when wrong is set).
func release(t *testing.T, dir, version string, wrong bool) {
	t.Helper()
	binary := build(t, t.TempDir(), version)
	name := fmt.Sprintf("verifyinboxes_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, file := range []struct {
		name string
		path string
		mode int64
	}{{"verifyinboxes", binary, 0o755}, {"README.md", "../../README.md", 0o644}, {"LICENSE", "../../LICENSE", 0o644}} {
		data, err := os.ReadFile(file.path)
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: file.mode, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []interface{ Close() error }{tw, gz, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if wrong {
		sum[0] ^= 0xff
	}
	checksums := fmt.Sprintf("%s  verifyinboxes_%s_windows_amd64.zip\n%s  %s\n", strings.Repeat("0", 64), version, hex.EncodeToString(sum[:]), name)
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(checksums), 0o644); err != nil {
		t.Fatal(err)
	}
}

// install runs install.sh against the release files in base, into installDir.
func install(t *testing.T, base, installDir, version string) (string, error) {
	t.Helper()
	script, err := filepath.Abs("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", script)
	cmd.Env = append(os.Environ(), "VERIFYINBOXES_DOWNLOAD_BASE=file://"+filepath.ToSlash(base),
		"VERIFYINBOXES_VERSION="+version, "VERIFYINBOXES_INSTALL_DIR="+installDir, "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestInstallScript installs a release from local files, as install.sh would from GitHub.
func TestInstallScript(t *testing.T) {
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
	release(t, base, "0.0.1-test", false)
	out, err := install(t, base, bin, "v0.0.1-test")
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Installed verifyinboxes 0.0.1-test") || !strings.HasSuffix(strings.TrimSpace(out), "Next:  verifyinboxes login") {
		t.Fatalf("install.sh said:\n%s", out)
	}
	version, err := exec.Command(filepath.Join(bin, "verifyinboxes"), "--version").Output()
	if err != nil || !strings.HasPrefix(string(version), "verifyinboxes 0.0.1-test") {
		t.Fatalf("the installed binary: %q %v", version, err)
	}

	// A download whose checksum does not match installs nothing.
	base, bin = t.TempDir(), filepath.Join(t.TempDir(), "bin")
	release(t, base, "0.0.2-test", true)
	out, err = install(t, base, bin, "0.0.2-test")
	if err == nil || !strings.Contains(out, "checksum") {
		t.Fatalf("a wrong checksum was taken:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(bin, "verifyinboxes")); err == nil {
		t.Fatal("a binary was installed after a wrong checksum")
	}
}
