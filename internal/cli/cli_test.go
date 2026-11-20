package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/verifyinboxes/verifyinboxes-cli/internal/fakeapi"
	"github.com/zalando/go-keyring"
)

// ---- harness ---------------------------------------------------------------------------------------------------

// syncBuffer is a buffer that the tool writes to while a test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type result struct {
	code      int
	out, errs string
}

// tester is one test's fake API and configuration directory, and the tool run against them.
type tester struct {
	t    *testing.T
	fake *fakeapi.Server
	dir  string

	mu    sync.Mutex
	pages []string
}

// secrets are what no output may hold.
var secrets = []string{"bv_", "vi_at_", "vi_rt_"}

func setup(t *testing.T, opts fakeapi.Options) *tester {
	t.Helper()
	fake := fakeapi.New(opts)
	t.Cleanup(fake.Close)
	dir := filepath.Join(t.TempDir(), "config")
	t.Setenv(envConfigDir, dir)
	t.Setenv(envBaseURL, fake.URL)
	t.Setenv(envAPIKey, "")
	return &tester{t: t, fake: fake, dir: dir}
}

// follow is the browser: it opens the address and follows the authorization page's redirect to the tool's
// loopback address, keeping the page it got there.
func (x *tester) follow(address string) error {
	go func() {
		resp, err := http.Get(address)
		if err != nil {
			x.mu.Lock()
			x.pages = append(x.pages, "error: "+err.Error())
			x.mu.Unlock()
			return
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		x.mu.Lock()
		x.pages = append(x.pages, string(body))
		x.mu.Unlock()
	}()
	return nil
}

// page waits for the page the browser got back from the tool.
func (x *tester) page() string {
	x.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		x.mu.Lock()
		if len(x.pages) > 0 {
			p := x.pages[len(x.pages)-1]
			x.mu.Unlock()
			return p
		}
		x.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	x.t.Fatal("the browser got no page back")
	return ""
}

func (x *tester) app(in io.Reader, out, errs io.Writer) *App {
	return &App{In: in, Out: out, Err: errs, Build: BuildInfo{Version: "1.2.3"}, OpenBrowser: x.follow,
		LoginTimeout: 10 * time.Second}
}

// run runs the tool with stdin and these arguments, and checks that nothing it printed holds a secret.
func (x *tester) run(stdin string, args ...string) result {
	x.t.Helper()
	var out, errs syncBuffer
	code := x.app(strings.NewReader(stdin), &out, &errs).Run(context.Background(), args)
	r := result{code: code, out: out.String(), errs: errs.String()}
	x.noSecret(args, r)
	return r
}

func (x *tester) noSecret(args []string, r result) {
	x.t.Helper()
	for _, s := range secrets {
		if strings.Contains(r.out, s) || strings.Contains(r.errs, s) {
			x.t.Errorf("verifyinboxes %v printed something starting %s:\nstdout: %s\nstderr: %s", args, s, r.out, r.errs)
		}
	}
}

func (x *tester) expect(code int, stdin string, args ...string) result {
	x.t.Helper()
	r := x.run(stdin, args...)
	if r.code != code {
		x.t.Fatalf("verifyinboxes %v exited %d, want %d\nstdout: %s\nstderr: %s", args, r.code, code, r.out, r.errs)
	}
	return r
}

// saved reads what the tool saved, by base URL.
func (x *tester) saved() map[string]savedEntry {
	x.t.Helper()
	data, err := os.ReadFile(filepath.Join(x.dir, "credentials.json"))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]savedEntry{}
	}
	if err != nil {
		x.t.Fatal(err)
	}
	var all map[string]savedEntry
	if err := json.Unmarshal(data, &all); err != nil {
		x.t.Fatalf("credentials.json is not JSON: %v", err)
	}
	return all
}

// save replaces what is saved for the fake's address.
func (x *tester) save(e savedEntry) {
	x.t.Helper()
	data, err := json.Marshal(map[string]savedEntry{x.fake.URL: e})
	if err != nil {
		x.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(x.dir, "credentials.json"), data, 0o600); err != nil {
		x.t.Fatal(err)
	}
}

// last returns the last request with this method and path.
func (x *tester) last(method, path string) fakeapi.Request {
	x.t.Helper()
	seen := x.fake.Seen()
	for i := len(seen) - 1; i >= 0; i-- {
		if seen[i].Method == method && seen[i].Path == path {
			return seen[i]
		}
	}
	x.t.Fatalf("no %s %s was sent", method, path)
	return fakeapi.Request{}
}

func lines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// jsonLines parses each line of s as one JSON object.
func jsonLines(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range lines(s) {
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("not one JSON object on a line: %q (%v)", line, err)
		}
		out = append(out, v)
	}
	return out
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ---- signing in ------------------------------------------------------------------------------------------------

func TestLoginSavesATokenReadableOnlyByYou(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	r := x.expect(0, "", "login")
	if r.out != "Signed in to Acme.\n" {
		t.Fatalf("stdout = %q", r.out)
	}
	if page := x.page(); !strings.Contains(page, "Signed in. You can close this tab.") || strings.Contains(page, "vi_") ||
		strings.Contains(page, "code") {
		t.Fatalf("the browser was shown %q", page)
	}

	// What it registered, and how it asked.
	var reg map[string]any
	if err := json.Unmarshal(x.last("POST", "/oauth/register").Body, &reg); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"client_name": "VerifyInboxes CLI", "redirect_uris": []any{"http://127.0.0.1/callback"},
		"token_endpoint_auth_method": "none", "application_type": "native", "software_id": "verifyinboxes-cli",
		"software_version": "1.2.3",
	}
	if fmt.Sprint(reg) != fmt.Sprint(want) {
		t.Fatalf("registered %v, want %v", reg, want)
	}
	q, _ := url.ParseQuery(x.last("GET", "/oauth/authorize").Query)
	redirect := q.Get("redirect_uri")
	if !regexp.MustCompile(`^http://127\.0\.0\.1:\d+/callback$`).MatchString(redirect) || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || len(q.Get("state")) != 43 ||
		q.Get("resource") != x.fake.URL+"/v1" || q.Get("client_id") == "" {
		t.Fatalf("the authorization request was %v", q)
	}
	form, _ := url.ParseQuery(string(x.last("POST", "/oauth/token").Body))
	if form.Get("grant_type") != "authorization_code" || len(form.Get("code_verifier")) != 64 ||
		form.Get("redirect_uri") != redirect || form.Get("resource") != x.fake.URL+"/v1" ||
		form.Get("client_id") != q.Get("client_id") {
		t.Fatalf("the token request was %v", form)
	}
	// The loopback listener is closed once the sign-in is over.
	if conn, err := net.DialTimeout("tcp", strings.TrimSuffix(strings.TrimPrefix(redirect, "http://"), "/callback"), time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("the loopback listener is still open")
	}

	// Saved for this address only, readable by you alone.
	e := x.saved()[x.fake.URL]
	if e.API == nil || !strings.HasPrefix(e.API.AccessToken, "vi_at_") || !strings.HasPrefix(e.API.RefreshToken, "vi_rt_") ||
		e.API.ClientID != q.Get("client_id") || time.Until(e.API.ExpiresAt) < 59*time.Minute || e.MCP != nil || e.APIKey != "" {
		t.Fatalf("saved %+v", e)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(x.dir, "credentials.json"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("credentials.json: %v %v", info.Mode(), err)
		}
		info, err = os.Stat(x.dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("the directory: %v %v", info.Mode(), err)
		}
	}
	if leftovers, _ := filepath.Glob(filepath.Join(x.dir, "*.tmp*")); len(leftovers) > 0 {
		t.Fatalf("a temporary file was left: %v", leftovers)
	}

	// The commands use it.
	x.expect(0, "", "account")
	if got := x.last("GET", "/v1/account").Authorization; got != e.API.AccessToken {
		t.Fatal("account did not use the saved access token")
	}

	// A sign-in for AI apps is its own, for /mcp.
	r = x.expect(0, "", "login", "--for", "mcp")
	if r.out != "Signed in for AI apps: verifyinboxes mcp now works.\n" {
		t.Fatalf("stdout = %q", r.out)
	}
	q, _ = url.ParseQuery(x.last("GET", "/oauth/authorize").Query)
	if q.Get("resource") != x.fake.URL+"/mcp" {
		t.Fatalf("login --for mcp asked for %q", q.Get("resource"))
	}
	e2 := x.saved()[x.fake.URL]
	if e2.MCP == nil || e2.API == nil || e2.API.AccessToken != e.API.AccessToken {
		t.Fatalf("saved %+v", e2)
	}
}

func TestLoginRefusesAWrongStateOrIssuer(t *testing.T) {
	for _, c := range []struct {
		name string
		opts fakeapi.Options
		says string
	}{
		{"state", fakeapi.Options{WrongState: true}, "state"},
		{"iss", fakeapi.Options{WrongIssuer: true}, "iss"},
		{"metadata issuer", fakeapi.Options{Issuer: "https://elsewhere.example"}, "issuer"},
		{"refused", fakeapi.Options{AuthorizeError: "access_denied"}, "access_denied"},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := setup(t, c.opts)
			r := x.expect(1, "", "login")
			if !strings.Contains(r.errs, c.says) || r.out != "" {
				t.Fatalf("stderr = %q, stdout = %q", r.errs, r.out)
			}
			if len(x.saved()) != 0 {
				t.Fatalf("something was saved: %+v", x.saved())
			}
			if x.fake.Count("POST", "/oauth/token") != 0 {
				t.Fatal("a code was exchanged")
			}
			if c.name == "metadata issuer" {
				if x.fake.Count("POST", "/oauth/register") != 0 {
					t.Fatal("it registered with a server that is not the issuer")
				}
				return
			}
			if page := x.page(); strings.Contains(page, "Signed in") {
				t.Fatalf("the browser was told %q", page)
			}
		})
	}
}

func TestNoBrowserTakesAPastedAddress(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	in, typed := io.Pipe()
	var out, errs syncBuffer
	app := x.app(in, &out, &errs)
	app.OpenBrowser = func(string) error {
		t.Error("--no-browser opened a browser")
		return nil
	}
	done := make(chan int, 1)
	go func() { done <- app.Run(context.Background(), []string{"login", "--no-browser"}) }()

	address := regexp.MustCompile(`http://127\.0\.0\.1:\d+/oauth/authorize\?\S+`)
	var shown string
	for deadline := time.Now().Add(5 * time.Second); shown == "" && time.Now().Before(deadline); {
		shown = address.FindString(errs.String())
		time.Sleep(10 * time.Millisecond)
	}
	if shown == "" {
		t.Fatalf("no address was printed: %q", errs.String())
	}
	// The browser is elsewhere: the redirect back to this computer never arrives, and the person copies the
	// address it ended on.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(shown)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	landed := resp.Header.Get("Location")
	if !strings.Contains(landed, "code=") {
		t.Fatalf("the authorization page sent the browser to %q", landed)
	}
	fmt.Fprintln(typed, "not an address")
	fmt.Fprintln(typed, "  "+landed+"  ")
	select {
	case code := <-done:
		if code != 0 || out.String() != "Signed in to Acme.\n" {
			t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out.String(), errs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("login did not take the pasted address")
	}
	_ = typed.Close()
	if !strings.Contains(errs.String(), "paste the whole address") {
		t.Fatalf("a wrong paste was not explained: %q", errs.String())
	}
	if strings.Contains(errs.String(), "vi_ac_") {
		t.Fatal("the code was printed")
	}
	x.noSecret(nil, result{out: out.String(), errs: errs.String()})
	if e := x.saved()[x.fake.URL]; e.API == nil {
		t.Fatal("nothing was saved")
	}
}

func TestKeyFromEnvironmentWins(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	x.expect(0, "", "login")
	x.expect(0, "", "login", "--for", "mcp")
	t.Setenv(envAPIKey, fakeapi.Key)
	x.expect(0, "", "account")
	if x.last("GET", "/v1/account").Authorization != fakeapi.Key {
		t.Fatal("the saved sign-in was used before VERIFYINBOXES_API_KEY")
	}
	in, typed := io.Pipe()
	var out, errs syncBuffer
	done := make(chan int, 1)
	go func() { done <- x.app(in, &out, &errs).Run(context.Background(), []string{"mcp"}) }()
	fmt.Fprintln(typed, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	_ = typed.Close()
	if code := <-done; code != 0 {
		t.Fatalf("mcp exited %d: %s", code, errs.String())
	}
	if x.last("POST", "/mcp").Authorization != fakeapi.Key {
		t.Fatal("mcp used the saved sign-in before VERIFYINBOXES_API_KEY")
	}
}

func TestLoginAPIKeyReadsStdinNotArgv(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	r := x.expect(0, "  "+fakeapi.Key+"  \n", "login", "--api-key")
	if r.out != "Saved the API key for Acme.\n" {
		t.Fatalf("stdout = %q", r.out)
	}
	if x.saved()[x.fake.URL].APIKey != fakeapi.Key {
		t.Fatal("the key was not saved")
	}
	x.expect(0, "", "account")
	if x.last("GET", "/v1/account").Authorization != fakeapi.Key {
		t.Fatal("the saved key was not used")
	}

	// A key on the command line is refused, and never repeated.
	for _, args := range [][]string{{"login", "--api-key", fakeapi.BadKey}, {"login", "--api-key=" + fakeapi.BadKey}} {
		r := x.run("", args...)
		if r.code != 2 {
			t.Fatalf("%v exited %d: %s", args, r.code, r.errs)
		}
	}

	// A key that does not work is not saved.
	r = x.run(fakeapi.BadKey+"\n", "login", "--api-key")
	if r.code != 1 || x.saved()[x.fake.URL].APIKey != fakeapi.Key {
		t.Fatalf("a refused key: exit %d, saved %+v, stderr %s", r.code, x.saved(), r.errs)
	}

	// On a terminal, it is asked for without showing it.
	var out, errs syncBuffer
	app := x.app(strings.NewReader("not read"), &out, &errs)
	app.InTTY = true
	asked := 0
	app.ReadSecret = func() (string, error) { asked++; return fakeapi.Key, nil }
	if code := app.Run(context.Background(), []string{"login", "--api-key"}); code != 0 || asked != 1 {
		t.Fatalf("exit %d, asked %d times: %s", code, asked, errs.String())
	}
	if !strings.Contains(errs.String(), "API key") || strings.Contains(errs.String(), "bv_") {
		t.Fatalf("stderr = %q", errs.String())
	}

	// With no key at all.
	if r := x.run("", "login", "--api-key"); r.code != 2 {
		t.Fatalf("an empty key: exit %d", r.code)
	}
}

func TestRefreshesBeforeExpiry(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	x.expect(0, "", "login")
	e := x.saved()[x.fake.URL]
	old := *e.API

	// Within a minute of its end, the token is renewed before it is used, and the new one saved.
	e.API.ExpiresAt = time.Now().Add(30 * time.Second)
	x.save(e)
	x.expect(0, "", "account")
	if asked, _ := x.fake.Refreshes(); asked != 1 {
		t.Fatalf("%d refreshes", asked)
	}
	form, _ := url.ParseQuery(string(x.last("POST", "/oauth/token").Body))
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != old.RefreshToken ||
		form.Get("client_id") != old.ClientID || form.Get("resource") != x.fake.URL+"/v1" {
		t.Fatalf("the refresh sent %v", form)
	}
	now := x.saved()[x.fake.URL].API
	if now.AccessToken == old.AccessToken || now.RefreshToken == old.RefreshToken || time.Until(now.ExpiresAt) < 59*time.Minute {
		t.Fatal("the renewed token was not saved")
	}
	if x.last("GET", "/v1/account").Authorization != now.AccessToken {
		t.Fatal("the request did not carry the renewed token")
	}

	// A token the API no longer takes (invalid_token) is renewed, and the request sent again.
	x.fake.ExpireAccessTokens()
	x.expect(0, "", "account")
	if asked, _ := x.fake.Refreshes(); asked != 2 {
		t.Fatalf("%d refreshes", asked)
	}
	if got := x.last("GET", "/v1/account"); got.Status != 200 || got.Authorization != x.saved()[x.fake.URL].API.AccessToken {
		t.Fatal("the request was not sent again with the renewed token")
	}
}

func TestTwoProcessesRefreshing(t *testing.T) {
	var first *tester
	var other result
	x := setup(t, fakeapi.Options{OnFirstRefresh: func() {
		// Another process renews the same sign-in while this one's refresh is on its way.
		other = first.run("", "account")
	}})
	first = x
	x.expect(0, "", "login")
	e := x.saved()[x.fake.URL]
	e.API.ExpiresAt = time.Now().Add(10 * time.Second)
	x.save(e)

	x.expect(0, "", "account")
	if other.code != 0 {
		t.Fatalf("the other process: exit %d: %s", other.code, other.errs)
	}
	asked, refused := x.fake.Refreshes()
	if asked != 2 || refused != 1 {
		t.Fatalf("%d refreshes, %d refused", asked, refused)
	}
	// This one's invalid_grant read the store again and took the token the other one saved.
	saved := x.saved()[x.fake.URL].API
	var calls []fakeapi.Request
	for _, r := range x.fake.Seen() {
		if r.Path == "/v1/account" {
			calls = append(calls, r)
		}
	}
	if len(calls) < 3 || calls[len(calls)-1].Authorization != saved.AccessToken || calls[len(calls)-2].Authorization != saved.AccessToken {
		t.Fatal("the two processes did not end on the one renewed token")
	}
	if x.fake.LiveGrants() != 1 {
		t.Fatalf("%d live sign-ins", x.fake.LiveGrants())
	}

	// With no newer one saved, it says to sign in again.
	form := url.Values{"client_id": {saved.ClientID}, "token": {saved.RefreshToken}}
	resp, err := http.PostForm(x.fake.URL+"/oauth/revoke", form)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	e = x.saved()[x.fake.URL]
	e.API.ExpiresAt = time.Now()
	x.save(e)
	r := x.expect(1, "", "account")
	if !strings.Contains(r.errs, "sign in again: verifyinboxes login") {
		t.Fatalf("stderr = %q", r.errs)
	}
}

func TestLogoutRevokesAndForgets(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	x.expect(0, "", "login")
	x.expect(0, "", "login", "--for", "mcp")
	e := x.saved()[x.fake.URL]
	r := x.expect(0, "", "logout")
	if r.out != "Signed out.\n" {
		t.Fatalf("stdout = %q", r.out)
	}
	revoked := strings.Join(x.fake.Revoked(), " ")
	if !strings.Contains(revoked, e.API.RefreshToken) || !strings.Contains(revoked, e.MCP.RefreshToken) {
		t.Fatal("the saved refresh tokens were not revoked")
	}
	if _, ok := x.saved()[x.fake.URL]; ok {
		t.Fatal("the sign-in is still saved")
	}
	r = x.expect(1, "", "account")
	if !strings.Contains(r.errs, "verifyinboxes login") {
		t.Fatalf("stderr = %q", r.errs)
	}
}

func TestKeychainHoldsTheSignInWhereThereIsOne(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	home := t.TempDir()
	t.Setenv(envConfigDir, "")
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "AppData", "APPDATA"} {
		t.Setenv(name, home)
	}
	keyring.MockInit()
	t.Cleanup(func() { keyring.MockInitWithError(errors.New("no keychain in the tests")) })
	r := x.expect(0, fakeapi.Key+"\n", "login", "--api-key")
	stored, err := keyring.Get(keyringService, x.fake.URL)
	if err != nil || !strings.Contains(stored, `"api_key"`) {
		t.Fatalf("the keychain holds %d characters (%v)", len(stored), err)
	}
	config, _ := os.UserConfigDir()
	path := filepath.Join(config, "verifyinboxes", "credentials.json")
	if !strings.HasPrefix(path, home) {
		t.Fatalf("the configuration directory %s is not the test's", path)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a file was written too")
	}
	if strings.Contains(r.errs, "credentials.json") {
		t.Fatalf("stderr = %q", r.errs)
	}

	// Without a keychain, a file, and a notice saying where.
	keyring.MockInitWithError(errors.New("no keychain"))
	r = x.expect(0, fakeapi.Key+"\n", "login", "--api-key")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no file at %s: %v", path, err)
	}
	if !strings.Contains(r.errs, path) {
		t.Fatalf("the notice does not say where: %q", r.errs)
	}
	x.expect(0, "", "account")
}

func TestKeychainThatFailsAfterItWorkedIsAnError(t *testing.T) {
	locked := errors.New("the keychain is locked")
	// The keychain breaks while a renewal is on its way, after this run has read the sign-in from it.
	x := setup(t, fakeapi.Options{OnFirstRefresh: func() { keyring.MockInitWithError(locked) }})
	home := t.TempDir()
	t.Setenv(envConfigDir, "")
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "AppData", "APPDATA"} {
		t.Setenv(name, home)
	}
	keyring.MockInit()
	t.Cleanup(func() { keyring.MockInitWithError(errors.New("no keychain in the tests")) })
	x.expect(0, "", "login")
	stored, err := keyring.Get(keyringService, x.fake.URL)
	var e savedEntry
	if err != nil || json.Unmarshal([]byte(stored), &e) != nil || e.API == nil {
		t.Fatalf("the keychain holds no sign-in (%v)", err)
	}
	e.API.ExpiresAt = time.Now().Add(30 * time.Second) // renewed before it is used
	data, _ := json.Marshal(e)
	if err := keyring.Set(keyringService, x.fake.URL, string(data)); err != nil {
		t.Fatal(err)
	}

	// The renewed sign-in cannot be saved where it was read from: that is an error, not a file the keychain's older
	// entry would hide from the next run (its refresh token already used up).
	r := x.expect(1, "", "account")
	if !strings.Contains(r.errs, "the keychain could not") || !strings.Contains(r.errs, locked.Error()) {
		t.Fatalf("stderr = %q", r.errs)
	}
	config, _ := os.UserConfigDir()
	if _, err := os.Stat(filepath.Join(config, "verifyinboxes", "credentials.json")); err == nil {
		t.Fatal("the renewed sign-in was written to a file instead")
	}
}

func TestKeychainThatHoldsNothingAndRefusesTheSaveFallsBackToTheFile(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	home := t.TempDir()
	t.Setenv(envConfigDir, "")
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "AppData", "APPDATA"} {
		t.Setenv(name, home)
	}
	// A keychain that holds nothing for this address and refuses to save (a locked one, or an entry too big for it):
	// the mock answers every call with this one error, so its Get finds nothing and its Set fails.
	keyring.MockInitWithError(keyring.ErrNotFound)
	t.Cleanup(func() { keyring.MockInitWithError(errors.New("no keychain in the tests")) })

	// No older entry in the keychain can hide the file from the next run: the first sign-in goes to the file.
	r := x.expect(0, fakeapi.Key+"\n", "login", "--api-key")
	config, _ := os.UserConfigDir()
	path := filepath.Join(config, "verifyinboxes", "credentials.json")
	if !strings.HasPrefix(path, home) {
		t.Fatalf("the configuration directory %s is not the test's", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no file at %s: %v (stderr %q)", path, err, r.errs)
	}
	x.expect(0, "", "account")
}

func TestLockIsTheHoldersOnly(t *testing.T) {
	wait, stale := lockWait, lockStale
	t.Cleanup(func() { lockWait, lockStale = wait, stale })
	// A lock is taken over only once a renewal's request could no longer be on its way.
	if lockStale <= oauthHTTP().Timeout {
		t.Fatalf("a lock is taken over after %v, before a renewal's %v timeout", lockStale, oauthHTTP().Timeout)
	}
	lockWait = 300 * time.Millisecond
	s := &credStore{file: filepath.Join(t.TempDir(), "credentials.json")}
	lockFile := s.file + ".lock"
	base := "https://api.example.com"
	save := func(e *savedEntry) { e.APIKey = fakeapi.Key }

	// Another run holds it: this one waits, then gives up with an error that names the lock, and saves nothing.
	if err := os.WriteFile(lockFile, []byte("another run's token"), 0o600); err != nil {
		t.Fatal(err)
	}
	var refused *cliError
	if err := s.update(base, save); !errors.As(err, &refused) || !strings.Contains(refused.msg, lockFile) {
		t.Fatalf("a held lock: %v", err)
	}
	if _, err := os.Stat(s.file); err == nil {
		t.Fatal("it saved without the lock")
	}

	// One left by a run that died is taken over, and released afterwards.
	old := time.Now().Add(-lockStale - time.Second)
	if err := os.Chtimes(lockFile, old, old); err != nil {
		t.Fatal(err)
	}
	if err := s.update(base, save); err != nil {
		t.Fatalf("a stale lock: %v", err)
	}
	if _, err := os.Stat(lockFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the lock was not released")
	}

	// A run whose lock another took over meanwhile leaves that one's lock alone.
	release, err := s.lock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockFile, []byte("the token of the run that took it over"), 0o600); err != nil {
		t.Fatal(err)
	}
	release()
	if held, err := os.ReadFile(lockFile); err != nil || string(held) != "the token of the run that took it over" {
		t.Fatalf("the other run's lock was released: %q %v", held, err)
	}
}

func TestStaleLockThatCannotBeRemovedIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory this user cannot write to")
	}
	wait := lockWait
	t.Cleanup(func() { lockWait = wait })
	lockWait = 300 * time.Millisecond
	dir := t.TempDir()
	s := &credStore{file: filepath.Join(dir, "credentials.json")}
	lockFile := s.file + ".lock"
	if err := os.WriteFile(lockFile, []byte("a run that died"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-lockStale - time.Second)
	if err := os.Chtimes(lockFile, old, old); err != nil {
		t.Fatal(err)
	}
	// A configuration directory that is read-only now: the lock left in it cannot be removed.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	done := make(chan error, 1)
	go func() {
		release, err := s.lock()
		if err == nil {
			release()
		}
		done <- err
	}()
	select {
	case err := <-done:
		var refused *cliError
		if !errors.As(err, &refused) || !strings.Contains(refused.msg, lockFile) {
			t.Fatalf("a stale lock that cannot be removed: %v", err)
		}
	case <-time.After(lockWait + 5*time.Second):
		t.Fatal("still trying to take the lock long after its wait")
	}
}

func TestSignInEndedSaysWhyOnlyWhenTold(t *testing.T) {
	if got := signInEnded(forAPI, &oauthError{Code: "invalid_grant"}).msg; got != "your sign-in has ended: sign in again: verifyinboxes login" {
		t.Errorf("without a description: %q", got)
	}
	told := &oauthError{Code: "invalid_grant", Description: "the refresh token was revoked"}
	if got := signInEnded(forMCP, told).msg; got != "your sign-in has ended (the refresh token was revoked): sign in again: verifyinboxes login --for mcp" {
		t.Errorf("with one: %q", got)
	}
}

// ---- checking addresses ----------------------------------------------------------------------------------------

func TestVerifyManyAddressesMakeOneList(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	t.Setenv(envAPIKey, fakeapi.Key)
	var emails []string
	for i := 1; i <= 11; i++ {
		emails = append(emails, fmt.Sprintf("p%d@example.com", i))
	}
	r := x.expect(0, strings.Join(emails, "\n")+"\n\n", "verify", "--json")
	if x.fake.Count("POST", "/v1/jobs") != 1 || x.fake.Count("POST", "/v1/verify") != 0 {
		t.Fatalf("%d lists and %d single checks", x.fake.Count("POST", "/v1/jobs"), x.fake.Count("POST", "/v1/verify"))
	}
	var sent struct {
		Emails []string `json:"emails"`
	}
	_ = json.Unmarshal(x.last("POST", "/v1/jobs").Body, &sent)
	if strings.Join(sent.Emails, ",") != strings.Join(emails, ",") {
		t.Fatalf("the list held %v", sent.Emails)
	}
	got := jsonLines(t, r.out)
	if len(got) != 11 {
		t.Fatalf("%d lines", len(got))
	}
	for i, g := range got {
		if g["email"] != emails[i] || g["verdict"] != "valid" || g["job_id"] == "" || g["job_id"] != got[0]["job_id"] {
			t.Fatalf("line %d: %v", i, g)
		}
	}

	// Ten or fewer are checks of their own.
	r = x.expect(0, "", "verify", "a@example.com", "bad@example.com")
	if x.fake.Count("POST", "/v1/verify") != 2 || x.fake.Count("POST", "/v1/jobs") != 1 {
		t.Fatal("two addresses did not make two checks")
	}
	if !strings.Contains(r.out, "a@example.com") || !strings.Contains(r.out, "mailbox_not_found") {
		t.Fatalf("stdout = %q", r.out)
	}
}

func TestFailOnExitCodes(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	t.Setenv(envAPIKey, fakeapi.Key)
	for _, c := range []struct {
		failOn, email string
		code          int
	}{
		{"invalid", "bad@example.com", 3},
		{"invalid", "catch@example.com", 0},
		{"unsafe", "catch@example.com", 3},
		{"unsafe", "bad@example.com", 3},
		{"invalid", "ok@example.com", 0},
		{"unsafe", "ok@example.com", 0},
	} {
		r := x.run("", "verify", "--fail-on", c.failOn, c.email)
		if r.code != c.code || !strings.Contains(r.out, c.email) {
			t.Fatalf("--fail-on %s %s: exit %d, want %d\n%s%s", c.failOn, c.email, r.code, c.code, r.out, r.errs)
		}
	}
	csvFile := writeFile(t, "prospects.csv", "name,work_email\nAnn,ann@example.com\nBad,bad@example.com\n")
	x.expect(3, "", "list", "run", csvFile, "--column", "work_email", "--fail-on", "invalid")
	x.expect(3, "", "list", "run", csvFile, "--column", "work_email", "--fail-on", "unsafe")
	good := writeFile(t, "good.csv", "email\nann@example.com\n")
	x.expect(0, "", "list", "run", good, "--fail-on", "unsafe")
	x.expect(2, "", "verify", "--fail-on", "maybe", "ok@example.com")
}

func TestJSONIsOneObjectPerLine(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	t.Setenv(envAPIKey, fakeapi.Key)
	csvFile := writeFile(t, "prospects.csv", "email\nann@example.com\nbad@example.com\ncatch@example.com\n")
	created := jsonLines(t, x.expect(0, "", "list", "run", csvFile, "--json").out)
	if len(created) != 1 || created[0]["id"] == nil || created[0]["total"] != float64(3) {
		t.Fatalf("list run --json: %v", created)
	}
	id := created[0]["id"].(string)
	for _, c := range []struct {
		args  []string
		lines int
	}{
		{[]string{"account"}, 1},
		{[]string{"verify", "a@example.com", "b@example.com"}, 2},
		{[]string{"list", "run", csvFile, "--wait"}, 1},
		{[]string{"list", "status", id}, 1},
		{[]string{"list", "wait", id}, 1},
		{[]string{"list", "results", id}, 3},
		{[]string{"list", "results", id, "--only", "safe-to-send"}, 1},
		{[]string{"list", "results", id, "--verdicts", "invalid,catch_all"}, 2},
		{[]string{"list", "resolve", id}, 1},
		{[]string{"domain", "example.com"}, 1},
		{[]string{"webhooks", "add", "https://hooks.example.com/vi", "--event", "job.completed"}, 1},
		{[]string{"webhooks", "add", "https://hooks.example.com/two"}, 1},
		{[]string{"webhooks", "list"}, 2},
		{[]string{"suppressions", "add", "--email", "a@example.com", "--domain", "example.org"}, 1},
		{[]string{"suppressions", "list"}, 1},
	} {
		r := x.expect(0, "", append(c.args, "--json")...)
		if got := jsonLines(t, r.out); len(got) != c.lines {
			t.Fatalf("%v --json: %d lines, want %d:\n%s", c.args, len(got), c.lines, r.out)
		}
	}
	hook := jsonLines(t, x.expect(0, "", "webhooks", "list", "--json").out)[0]["id"].(string)
	x.expect(0, "", "webhooks", "delete", hook)
	if x.fake.Count("DELETE", "/v1/webhooks/"+hook) != 1 {
		t.Fatal("webhooks delete sent nothing")
	}
	// Human output too, for the commands that print a secret once.
	r := x.expect(0, "", "webhooks", "add", "https://hooks.example.com/three")
	if !strings.Contains(r.out, strings.Repeat("ab", 32)) || !strings.Contains(r.out, "store it now") {
		t.Fatalf("webhooks add: %q", r.out)
	}
	// list download: the CSV on stdout, or a file.
	r = x.expect(0, "", "list", "download", id)
	if !strings.HasPrefix(r.out, "email,verdict,safe_to_send,reason\n") {
		t.Fatalf("list download: %q", r.out)
	}
	out := filepath.Join(t.TempDir(), "out.csv")
	x.expect(0, "", "list", "download", id, "-o", out, "--verdicts", "valid")
	if data, _ := os.ReadFile(out); string(data) != "email,verdict,safe_to_send,reason\nann@example.com,valid,true,mailbox_exists\n" {
		t.Fatalf("the file holds %q", data)
	}
}

func TestUsageErrorsExit2(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	t.Setenv(envAPIKey, fakeapi.Key)
	csvFile := writeFile(t, "prospects.csv", "email\nann@example.com\n")
	for _, args := range [][]string{
		{"nonsense"},
		{"--nope"},
		{"account", "extra"},
		{"verify", "--fail-on", "sometimes", "a@example.com"},
		{"verify", "--timeout", "soon", "a@example.com"},
		{"list", "run"},
		{"list", "run", csvFile, "--only", "safe-to-send", "--verdicts", "valid"},
		{"list", "run", csvFile, "--only", "everything"},
		{"list", "status"},
		{"list", "nonsense"},
		{"webhooks", "add"},
		{"suppressions", "add"},
		{"domain"},
		{"login", "--for", "elsewhere"},
		{"login", "extra"},
	} {
		if r := x.run("", args...); r.code != 2 {
			t.Errorf("%v exited %d, want 2\nstderr: %s", args, r.code, r.errs)
		} else if !strings.HasPrefix(r.errs, "error: ") {
			t.Errorf("%v: stderr = %q", args, r.errs)
		}
	}
	// verify with no addresses, on a terminal.
	var out, errs syncBuffer
	app := x.app(strings.NewReader(""), &out, &errs)
	app.InTTY = true
	if code := app.Run(context.Background(), []string{"verify"}); code != 2 {
		t.Fatalf("verify with nothing to check: exit %d", code)
	}
	if len(x.fake.Seen()) != 0 {
		t.Fatalf("a mistake in the command line sent %d requests", len(x.fake.Seen()))
	}
	if r := x.expect(0, "", "--version"); r.out != "verifyinboxes 1.2.3\n" {
		t.Fatalf("--version: %q", r.out)
	}
}

// ---- errors keep what a person needs (Review Focus 2 and 4) -----------------------------------------------------

func TestWaitTimeoutNamesTheJob(t *testing.T) {
	x := setup(t, fakeapi.Options{ListsStayRunning: true})
	t.Setenv(envAPIKey, fakeapi.Key)
	var emails []string
	for i := 1; i <= 11; i++ {
		emails = append(emails, fmt.Sprintf("p%d@example.com", i))
	}
	started := time.Now()
	r := x.expect(1, strings.Join(emails, "\n"), "verify", "--timeout", "1s")
	if time.Since(started) > 10*time.Second {
		t.Fatal("the wait did not stop at its timeout")
	}
	listID := "" // the list the verify made, as its waits asked for it
	for _, s := range x.fake.Seen() {
		if s.Method == "GET" && strings.HasPrefix(s.Path, "/v1/jobs/") {
			listID = strings.TrimPrefix(s.Path, "/v1/jobs/")
		}
	}
	if x.fake.Count("POST", "/v1/jobs") != 1 || listID == "" || !strings.Contains(r.errs, listID) || !strings.Contains(r.errs, "verifyinboxes list wait "+listID) {
		t.Fatalf("stderr does not name the list %q and how to wait for it: %q", listID, r.errs)
	}

	// The same for list run --wait and list wait.
	csvFile := writeFile(t, "prospects.csv", "email\nann@example.com\n")
	r = x.expect(1, "", "list", "run", csvFile, "--wait", "--timeout", "1s")
	if !strings.Contains(r.errs, "verifyinboxes list wait ") {
		t.Fatalf("list run --wait: %q", r.errs)
	}
	r = x.expect(1, "", "list", "wait", listID, "--timeout", "1s")
	if !strings.Contains(r.errs, "verifyinboxes list wait "+listID) {
		t.Fatalf("list wait: %q", r.errs)
	}
}

func TestRefusalNamesTheJobsMade(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	t.Setenv(envAPIKey, fakeapi.Key)
	r := x.expect(1, "", "verify", "ann@example.com", "broke@example.com")
	refused := x.last("POST", "/v1/verify")
	for _, s := range x.fake.Seen() {
		if s.Status == 402 {
			refused = s
		}
	}
	made := "019a0000-0000-7000-8000-000000000001" // ann's check, which carries on
	if !strings.Contains(r.errs, "insufficient_credits, HTTP 402, request id "+refused.RequestID) ||
		!strings.Contains(r.errs, "verifyinboxes list wait "+made) {
		t.Fatalf("stderr = %q", r.errs)
	}
}

func TestInterruptNamesTheJob(t *testing.T) {
	x := setup(t, fakeapi.Options{ListsStayRunning: true})
	t.Setenv(envAPIKey, fakeapi.Key)
	var emails []string
	for i := 1; i <= 11; i++ {
		emails = append(emails, fmt.Sprintf("p%d@example.com", i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	var out, errs syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- x.app(strings.NewReader(strings.Join(emails, "\n")), &out, &errs).Run(ctx, []string{"verify"})
	}()
	for deadline := time.Now().Add(5 * time.Second); x.fake.Count("POST", "/v1/jobs") == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	code := <-done
	if code != 130 || !strings.Contains(errs.String(), "verifyinboxes list wait 019a") {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
}

func TestErrorsNameTheRequest(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	t.Setenv(envAPIKey, fakeapi.BadKey)
	r := x.expect(1, "", "account")
	refused := x.last("GET", "/v1/account")
	if refused.Status != 401 || !strings.Contains(r.errs, "request id "+refused.RequestID) || !strings.Contains(r.errs, "unauthorized") ||
		!strings.Contains(r.errs, "HTTP 401") {
		t.Fatalf("401: stderr = %q", r.errs)
	}

	t.Setenv(envAPIKey, fakeapi.Key)
	r = x.expect(1, "", "list", "status", "no-such-list")
	missing := x.last("GET", "/v1/jobs/no-such-list")
	if !strings.Contains(r.errs, "request id "+missing.RequestID) || !strings.Contains(r.errs, "not_found") ||
		!strings.Contains(r.errs, "HTTP 404") {
		t.Fatalf("404: stderr = %q", r.errs)
	}

	r = x.expect(1, "", "domain", fakeapi.FailingDomain)
	broken := x.last("GET", "/v1/domains/"+fakeapi.FailingDomain)
	if x.fake.Count("GET", "/v1/domains/"+fakeapi.FailingDomain) != 3 || !strings.Contains(r.errs, "request id "+broken.RequestID) ||
		!strings.Contains(r.errs, "HTTP 502") {
		t.Fatalf("502: stderr = %q", r.errs)
	}
	if first := lines(r.errs)[0]; !strings.HasPrefix(first, "error: ") {
		t.Fatalf("the first line is %q", first)
	}
}

// TestTheAPIsTextCannotDriveTheTerminal: a server (one --base-url points at) whose codes, request ids and job ids
// hold an escape sequence and a bidirectional override gets neither onto the terminal.
func TestTheAPIsTextCannotDriveTheTerminal(t *testing.T) {
	const hostile = "\x1b[2J\u202e"
	job, list := "019a"+hostile+"-job", "019a"+hostile+"-list"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		// net/http refuses a control character in a header, whoever sends it: the request id has the override only.
		w.Header().Set("X-Request-Id", "req-\u202e-1")
		answer := func(status int, v any) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(v)
		}
		refusal := map[string]any{"error": map[string]any{"code": "refused" + hostile, "message": "no" + hostile}}
		switch {
		case r.URL.Path == "/v1/verify" && bytes.Contains(body, []byte("ann@")):
			answer(200, map[string]any{"email": "ann@example.com", "verdict": "valid", "safe_to_send": true, "job_id": job})
		case r.URL.Path == "/v1/verify":
			answer(402, refusal)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs":
			answer(201, map[string]any{"id": list, "status": "running", "total": 11})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/jobs/"):
			answer(200, map[string]any{"id": list, "kind": "bulk", "status": "running", "total": 11, "done": 0,
				"created_at": "2026-11-18T12:00:00Z", "purged": false})
		default:
			answer(500, refusal)
		}
	}))
	t.Cleanup(api.Close)
	t.Setenv(envConfigDir, filepath.Join(t.TempDir(), "config"))
	t.Setenv(envBaseURL, api.URL)
	t.Setenv(envAPIKey, fakeapi.Key)
	x := &tester{t: t}
	var emails []string
	for i := 1; i <= 11; i++ {
		emails = append(emails, fmt.Sprintf("p%d@example.com", i))
	}
	runs := []result{
		x.expect(1, "", "verify", "ann@example.com", "broke@example.com"),                 // an error naming a job
		x.expect(1, strings.Join(emails, "\n"), "verify", "--timeout", "1s"),              // a note, progress, a timeout
		x.expect(0, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n", "mcp"), // the bridge's log
	}
	for i, r := range runs {
		for _, bad := range []string{"\x1b", "\u202e"} {
			if strings.Contains(r.out, bad) || strings.Contains(r.errs, bad) {
				t.Errorf("run %d printed %q:\nstdout: %q\nstderr: %q", i, bad, r.out, r.errs)
			}
		}
	}
	// What is printed still names the jobs and the request, without what was dropped.
	if !strings.Contains(runs[0].errs, "verifyinboxes list wait 019a[2J-job") || !strings.Contains(runs[0].errs, "request id req--1") ||
		!strings.Contains(runs[0].errs, "refused[2J") {
		t.Errorf("stderr = %q", runs[0].errs)
	}
	if !strings.Contains(runs[1].errs, "as list 019a[2J-list") || !strings.Contains(runs[1].errs, "019a[2J-list: 0 of 11 checked") ||
		!strings.Contains(runs[1].errs, "verifyinboxes list wait 019a[2J-list") {
		t.Errorf("stderr = %q", runs[1].errs)
	}
	if !strings.Contains(runs[2].errs, "verifyinboxes mcp: no[2J") {
		t.Errorf("stderr = %q", runs[2].errs)
	}
}

func TestCleanDropsControlAndFormatCharacters(t *testing.T) {
	if got := clean("a\x1b[2Jb\u202ec\u200bd\u2066e\r\nf\u00e9"); got != "a[2Jbcdef\u00e9" {
		t.Errorf("clean: %q", got)
	}
	if got := clean("a\u202ab\u202dc\u200ed\u200fe\u061cf\ufeffg\u2067h\u2069i\u009bj"); got != "abcdefghij" {
		t.Errorf("clean: %q", got)
	}
}

func TestCleanKeepsTheFormatCharactersScriptsAreWrittenWith(t *testing.T) {
	for _, text := range []string{
		"user@\u0645\u06cc\u200c\u0631\u0648.ir", // a Persian IDN address with a zero-width non-joiner
		"\u0915\u094d\u200d\u0937",               // a Devanagari conjunct made with a zero-width joiner
		"co\u00adoperative",                      // a soft hyphen
		"\U0001F3F4\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074\U000E007F", // an emoji flag's tag characters
	} {
		if got := clean(text); got != text {
			t.Errorf("clean(%q) = %q", text, got)
		}
	}
	if got := clean("user@\u0645\u06cc\u200c\u0631\u0648.ir\u202e"); got != "user@\u0645\u06cc\u200c\u0631\u0648.ir" {
		t.Errorf("clean kept a right-to-left override: %q", got)
	}
}

// ---- mcp -------------------------------------------------------------------------------------------------------

// waitLines waits until out holds at least n lines, and returns them.
func waitLines(t *testing.T, out *syncBuffer, n int) []string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if got := lines(out.String()); len(got) >= n {
			return got
		}
	}
	t.Fatalf("waited for %d lines, got: %q", n, out.String())
	return nil
}

func TestMcpBridgesHeadersAndLines(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	t.Setenv(envAPIKey, fakeapi.Key)
	in, typed := io.Pipe()
	var out, errs syncBuffer
	done := make(chan int, 1)
	go func() { done <- x.app(in, &out, &errs).Run(context.Background(), []string{"mcp"}) }()
	send := func(line string) {
		wrote := make(chan error, 1)
		go func() { _, err := fmt.Fprintln(typed, line); wrote <- err }()
		select {
		case err := <-wrote:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("mcp does not read its input")
		}
	}
	answers := func(n int) map[string]map[string]any {
		got := map[string]map[string]any{}
		for _, line := range waitLines(t, &out, n) {
			var v map[string]any
			if err := json.Unmarshal([]byte(line), &v); err != nil {
				t.Fatalf("not protocol on stdout: %q", line)
			}
			got[fmt.Sprint(v["id"])] = v
		}
		return got
	}
	headersOf := func(a map[string]any) map[string]any {
		return a["result"].(map[string]any)["headers"].(map[string]any)
	}

	// A 2025-era client: initialize, then the version it negotiated on each request.
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	got := answers(1)
	h := headersOf(got["1"])
	if h["content-type"] != "application/json" || h["accept"] != "application/json, text/event-stream" ||
		!strings.HasPrefix(fmt.Sprint(h["user-agent"]), "verifyinboxes-cli/1.2.3") {
		t.Fatalf("initialize was sent with %v", h)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	got = answers(2)
	if h := headersOf(got["2"]); h["mcp-protocol-version"] != "2025-11-25" || h["mcp-method"] != nil {
		t.Fatalf("a 2025-era request was sent with %v", h)
	}

	// A 2026-07-28 client: the version, method and tool name from the message.
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_account","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`)
	got = answers(3)
	if h := headersOf(got["3"]); h["mcp-protocol-version"] != "2026-07-28" || h["mcp-method"] != "tools/call" || h["mcp-name"] != "get_account" {
		t.Fatalf("a modern request was sent with %v", h)
	}

	// Two in flight answer in any order; an event stream's messages are a line each; a failure answers the request.
	send(`{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{"delay_ms":400}}`)
	send(`{"jsonrpc":"2.0","id":5,"method":"tools/list"}`)
	got = answers(4)
	if _, ok := got["5"]; !ok {
		t.Fatalf("the quick call waited for the slow one: %v", got)
	}
	send(`{"jsonrpc":"2.0","id":6,"method":"stream"}`)
	send(`{"jsonrpc":"2.0","id":7,"method":"boom"}`)
	all := waitLines(t, &out, 8)
	got = answers(8)
	if _, ok := got["4"]; !ok {
		t.Fatalf("the slow call was never answered: %v", all)
	}
	if got["6"]["result"] == nil || got["<nil>"]["method"] != "notifications/progress" {
		t.Fatalf("the event stream: %v", all)
	}
	boom := x.last("POST", "/mcp")
	for _, s := range x.fake.Seen() {
		if s.Status == 500 {
			boom = s
		}
	}
	e, _ := got["7"]["error"].(map[string]any)
	if e == nil || !strings.Contains(fmt.Sprint(e["message"]), "something broke") ||
		!strings.Contains(fmt.Sprint(e["message"]), boom.RequestID) {
		t.Fatalf("a failed call was answered %v", got["7"])
	}
	_ = typed.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("mcp exited %d: %s", code, errs.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mcp did not end when stdin closed")
	}
	// Seven requests and a notification, nothing else on stdout; the notification wrote nothing.
	if n := len(lines(out.String())); n != 8 {
		t.Fatalf("%d lines on stdout: %s", n, out.String())
	}
	if x.fake.Count("POST", "/mcp") != 8 {
		t.Fatalf("%d requests to /mcp", x.fake.Count("POST", "/mcp"))
	}
}

func TestMcpUsesItsOwnSignIn(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	x.expect(0, "", "login", "--for", "mcp")
	x.fake.ExpireAccessTokens()
	r := x.expect(0, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n", "mcp")
	if got := jsonLines(t, r.out); len(got) != 1 || got[0]["result"] == nil {
		t.Fatalf("stdout = %q, stderr = %q", r.out, r.errs)
	}
	if asked, _ := x.fake.Refreshes(); asked != 1 || x.last("POST", "/mcp").Authorization != x.saved()[x.fake.URL].MCP.AccessToken {
		t.Fatal("a refused token was not renewed and sent again")
	}
}

func TestMcpWithoutCredentialSaysHow(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	x.expect(0, "", "login") // a sign-in for the API is not one for AI apps
	r := x.expect(1, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n", "mcp")
	if r.out != "" || !strings.Contains(r.errs, `run "verifyinboxes login --for mcp" once, or set VERIFYINBOXES_API_KEY`) {
		t.Fatalf("stdout = %q, stderr = %q", r.out, r.errs)
	}
	if x.fake.Count("POST", "/mcp") != 0 {
		t.Fatal("it sent a request without a credential")
	}
}

func TestMcpStreamThatEndsUnansweredAnswersTheRequest(t *testing.T) {
	x := setup(t, fakeapi.Options{})
	t.Setenv(envAPIKey, fakeapi.Key)
	r := x.expect(0, `{"jsonrpc":"2.0","id":9,"method":"stream_unanswered"}`+"\n", "mcp")
	got := jsonLines(t, r.out)
	if len(got) != 2 || got[0]["method"] != "notifications/progress" {
		t.Fatalf("stdout = %q", r.out)
	}
	e, _ := got[1]["error"].(map[string]any)
	if fmt.Sprint(got[1]["id"]) != "9" || e == nil || !strings.Contains(fmt.Sprint(e["message"]), "ended without an answer") ||
		!strings.Contains(fmt.Sprint(e["message"]), x.last("POST", "/mcp").RequestID) {
		t.Fatalf("the request was answered %v", got[1])
	}
}

// ---- small things ----------------------------------------------------------------------------------------------

func TestReadmeNamesEveryCommand(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Build: BuildInfo{Version: "1.2.3"}}
	for _, c := range app.rootCommand().Commands() {
		if c.Hidden || c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		if !bytes.Contains(readme, []byte("verifyinboxes "+c.Name())) {
			t.Errorf("README.md does not show verifyinboxes %s", c.Name())
		}
	}
	scanner := bufio.NewScanner(bytes.NewReader(readme))
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), "/home/") {
			t.Errorf("README.md names a home directory: %q", scanner.Text())
		}
	}
}
