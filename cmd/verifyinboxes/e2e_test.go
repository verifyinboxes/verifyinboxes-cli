package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/verifyinboxes/verifyinboxes-cli/internal/fakeapi"
)

// build compiles the tool into dir with this version, and returns the binary's path.
func build(t *testing.T, dir, version string) string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on the PATH")
	}
	binary := filepath.Join(dir, "verifyinboxes")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	cmd := exec.Command(goTool, "build", "-trimpath", "-ldflags", "-s -w -X main.version="+version+" -X main.commit=abc1234",
		"-o", binary, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return binary
}

// session runs the built binary against a fake API.
type session struct {
	t      *testing.T
	binary string
	env    []string
	dir    string
}

func (s *session) run(stdin string, args ...string) (string, string, int) {
	s.t.Helper()
	cmd := exec.Command(s.binary, args...)
	cmd.Env = s.env
	cmd.Dir = s.dir
	cmd.Stdin = strings.NewReader(stdin)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			s.t.Fatalf("verifyinboxes %v: %v", args, err)
		}
		code = exit.ExitCode()
	}
	for _, text := range []string{out.String(), errs.String()} {
		for _, secret := range []string{"bv_", "vi_at_", "vi_rt_"} {
			if strings.Contains(text, secret) {
				s.t.Errorf("verifyinboxes %v printed something starting %s", args, secret)
			}
		}
	}
	return out.String(), errs.String(), code
}

func (s *session) expect(code int, stdin string, args ...string) (string, string) {
	s.t.Helper()
	out, errs, got := s.run(stdin, args...)
	if got != code {
		s.t.Fatalf("verifyinboxes %v exited %d, want %d\nstdout: %s\nstderr: %s", args, got, code, out, errs)
	}
	return out, errs
}

// TestBinary builds the tool and runs it against the fake API as a person or a script would.
func TestBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	dir := t.TempDir()
	binary := build(t, dir, "9.8.7")
	fake := fakeapi.New(fakeapi.Options{})
	defer fake.Close()
	s := &session{t: t, binary: binary, dir: dir, env: append(os.Environ(),
		"VERIFYINBOXES_API_KEY="+fakeapi.Key, "VERIFYINBOXES_BASE_URL="+fake.URL,
		"VERIFYINBOXES_CONFIG_DIR="+filepath.Join(dir, "config"), "NO_COLOR=1")}

	if out, _ := s.expect(0, "", "--version"); out != "verifyinboxes 9.8.7 (abc1234)\n" {
		t.Fatalf("--version: %q", out)
	}

	out, _ := s.expect(0, "", "verify", "ann@example.com", "--json")
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil || result["verdict"] != "valid" || result["job_id"] == "" {
		t.Fatalf("verify --json: %q", out)
	}
	if ua := fake.Seen()[0].UserAgent; !strings.HasPrefix(ua, "verifyinboxes-cli/9.8.7 verifyinboxes-go/") {
		t.Fatalf("User-Agent %q", ua)
	}

	if err := os.WriteFile(filepath.Join(dir, "prospects.csv"), []byte("name,email\nAnn,ann@example.com\nBob,bad@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errs := s.expect(0, "", "list", "run", "prospects.csv", "-o", "results.csv")
	if !strings.Contains(errs, "2 of 2") && !strings.Contains(errs, "Started list") {
		t.Fatalf("list run showed no progress: %q", errs)
	}
	data, err := os.ReadFile(filepath.Join(dir, "results.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if header := strings.SplitN(string(data), "\n", 2)[0]; !strings.Contains(header, "verdict") || !strings.HasPrefix(header, "name,email") {
		t.Fatalf("results.csv starts %q", header)
	}

	out, _ = s.expect(0, "", "account")
	if !strings.Contains(out, "Acme") {
		t.Fatalf("account: %q", out)
	}
	if _, errs, code := s.run("", "list", "status", "no-such-list"); code != 1 || !strings.Contains(errs, "request id req-") {
		t.Fatalf("an unknown list: exit %d, %q", code, errs)
	}
	if _, _, code := s.run("", "--no-such-flag"); code != 2 {
		t.Fatalf("a wrong flag: exit %d", code)
	}
}
