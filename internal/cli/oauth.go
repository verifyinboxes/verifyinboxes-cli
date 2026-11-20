package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// The sign-in is OAuth 2.1's authorization code grant with PKCE, for a native app (RFC 8252): the tool registers
// itself (RFC 7591), listens on 127.0.0.1 for the browser to come back, and exchanges the code for tokens bound to
// one resource (RFC 8707): <base>/v1 for the API commands, <base>/mcp for `verifyinboxes mcp`.

// authServer is the API's authorization server, as its metadata (RFC 8414) describes it.
type authServer struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
	RevocationEndpoint    string `json:"revocation_endpoint"`

	http      *http.Client
	userAgent string
}

// oauthError is an error answer of the authorization server: {"error", "error_description"}.
type oauthError struct {
	Status      int
	Code        string
	Description string
	RequestID   string
}

func (e *oauthError) Error() string {
	msg := e.Code
	if e.Description != "" {
		msg += ": " + e.Description
	}
	return msg
}

// tokenAnswer is the token endpoint's answer.
type tokenAnswer struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func (t *tokenAnswer) lifetime() time.Duration {
	if t.ExpiresIn <= 0 {
		return time.Hour
	}
	return time.Duration(t.ExpiresIn) * time.Second
}

func isWindows() bool { return runtime.GOOS == "windows" }

// oauthHTTP is the client the sign-in's requests go through: it never follows a redirect.
func oauthHTTP() *http.Client {
	return &http.Client{Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// discover reads the authorization server's metadata, whose issuer must be the base URL itself.
func (a *App) discover(ctx context.Context, base string) (*authServer, error) {
	s := &authServer{http: oauthHTTP(), userAgent: a.userAgent()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/.well-known/oauth-authorization-server", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", s.userAgent)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, &cliError{msg: "no answer from " + base + ": " + causeOf(err), exit: exitError,
			hint: "Check your connection and --base-url, then try again."}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	requestID := resp.Header.Get("X-Request-Id")
	if resp.StatusCode == http.StatusNotFound {
		return nil, &cliError{msg: base + " has no sign-in", status: resp.StatusCode, requestID: requestID, exit: exitError,
			hint: "Save an organisation's API key instead: verifyinboxes login --api-key"}
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, s) != nil {
		return nil, &cliError{msg: "the sign-in's description could not be read from " + base, status: resp.StatusCode,
			requestID: requestID, exit: exitError}
	}
	if s.Issuer != base {
		return nil, &cliError{msg: fmt.Sprintf("the server at %s names another issuer (%s): refusing to sign in there", base,
			clean(s.Issuer)), exit: exitError, hint: "If the issuer is the API's address, give it with --base-url."}
	}
	for _, endpoint := range []string{s.AuthorizationEndpoint, s.TokenEndpoint, s.RegistrationEndpoint} {
		if u, err := url.Parse(endpoint); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, &cliError{msg: "the sign-in's description from " + base + " is incomplete", exit: exitError}
		}
	}
	return s, nil
}

func causeOf(err error) string {
	var u *url.Error
	if errors.As(err, &u) && u.Err != nil {
		return u.Err.Error()
	}
	return err.Error()
}

// post sends a request to the authorization server and reads its JSON answer into out; an error answer is an
// *oauthError.
func (s *authServer) post(ctx context.Context, endpoint, contentType string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", s.userAgent)
	resp, err := s.http.Do(req)
	if err != nil {
		return &cliError{msg: "no answer from the sign-in server: " + causeOf(err), exit: exitError,
			hint: "Check your connection, then try again."}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &oauthError{Status: resp.StatusCode, RequestID: resp.Header.Get("X-Request-Id")}
		var shaped struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		if json.Unmarshal(data, &shaped) == nil && shaped.Error != "" {
			e.Code, e.Description = clean(shaped.Error), clean(shaped.Description)
		} else {
			e.Code = fmt.Sprintf("the server answered %d", resp.StatusCode)
		}
		return e
	}
	if out != nil && len(data) > 0 && json.Unmarshal(data, out) != nil {
		return &cliError{msg: "the sign-in server's answer could not be read", requestID: resp.Header.Get("X-Request-Id"),
			status: resp.StatusCode, exit: exitError}
	}
	return nil
}

func formOf(fields map[string]string) []byte {
	form := url.Values{}
	for k, v := range fields {
		form.Set(k, v)
	}
	return []byte(form.Encode())
}

// token asks the token endpoint for tokens.
func (s *authServer) token(ctx context.Context, fields map[string]string) (*tokenAnswer, error) {
	var t tokenAnswer
	if err := s.post(ctx, s.TokenEndpoint, "application/x-www-form-urlencoded", formOf(fields), &t); err != nil {
		return nil, err
	}
	if !visible(t.AccessToken) || !visible(t.RefreshToken) {
		return nil, &cliError{msg: "the sign-in server answered without tokens", exit: exitError}
	}
	return &t, nil
}

// register makes the tool a client of the authorization server: once per sign-in.
func (s *authServer) register(ctx context.Context, version string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name": "VerifyInboxes CLI", "redirect_uris": []string{"http://127.0.0.1/callback"},
		"token_endpoint_auth_method": "none", "application_type": "native", "software_id": "verifyinboxes-cli",
		"software_version": version,
	})
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := s.post(ctx, s.RegistrationEndpoint, "application/json", body, &out); err != nil {
		return "", err
	}
	if out.ClientID == "" {
		return "", &cliError{msg: "the sign-in server registered the tool without a client_id", exit: exitError}
	}
	return out.ClientID, nil
}

// revoke ends the grant a refresh token belongs to.
func (s *authServer) revoke(ctx context.Context, clientID, refreshToken string) error {
	if s.RevocationEndpoint == "" {
		return errors.New("the server has no revocation endpoint")
	}
	return s.post(ctx, s.RevocationEndpoint, "application/x-www-form-urlencoded", formOf(map[string]string{
		"client_id": clientID, "token": refreshToken, "token_type_hint": "refresh_token",
	}), nil)
}

// oauthFailure is an authorization server's error as the tool reports it.
func oauthFailure(what string, err error) error {
	var o *oauthError
	if errors.As(err, &o) {
		e := &cliError{msg: what + ": " + o.Error(), requestID: o.RequestID, status: o.Status, exit: exitError}
		if o.Status == http.StatusTooManyRequests {
			e.hint = "Too many sign-ins from this address: wait a minute, then try again."
		}
		return e
	}
	return err
}

// randomText is n random bytes, base64url without padding.
func randomText(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("no randomness: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// callback is the browser coming back to the loopback address; reply says whether the sign-in worked, so that the
// page can say so.
type callback struct {
	query url.Values
	reply chan bool
}

const (
	pageSignedIn = "Signed in. You can close this tab."
	pageFailed   = "The sign-in did not finish. Go back to the terminal."
	pageDone     = "This sign-in has already finished. You can close this tab."
)

// page writes the one sentence the browser is shown: nothing that was in the address, and nothing loaded from
// anywhere.
func page(w http.ResponseWriter, status int, text string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!doctype html>\n<html><head><meta charset=\"utf-8\"><title>VerifyInboxes</title></head>"+
		"<body><p>%s</p></body></html>\n", text)
}

// browserLogin signs the person in with their browser and returns the tokens.
func (a *App) browserLogin(ctx context.Context, base string, kind resourceKind, noBrowser bool) (*OAuthCredential, *authServer, error) {
	auth, err := a.discover(ctx, base)
	if err != nil {
		return nil, nil, err
	}
	clientID, err := auth.register(ctx, a.Build.Version)
	if err != nil {
		return nil, nil, oauthFailure("registering the tool failed", err)
	}

	// The loopback address, on 127.0.0.1 only, at whatever port is free (RFC 8252 §7.3).
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, &cliError{msg: "cannot listen on 127.0.0.1 for the browser: " + err.Error(), exit: exitError}
	}
	port := listener.Addr().(*net.TCPAddr).Port
	host := fmt.Sprintf("127.0.0.1:%d", port)
	redirectURI := "http://" + host + "/callback"
	callbacks := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host || r.Method != http.MethodGet {
			page(w, http.StatusBadRequest, pageFailed)
			return
		}
		cb := callback{query: r.URL.Query(), reply: make(chan bool, 1)}
		select {
		case callbacks <- cb:
		default:
			page(w, http.StatusOK, pageDone)
			return
		}
		select {
		case ok := <-cb.reply:
			if ok {
				page(w, http.StatusOK, pageSignedIn)
			} else {
				page(w, http.StatusOK, pageFailed)
			}
		case <-r.Context().Done():
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		// The page is written before the listener closes; then nothing listens any more.
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		_ = server.Close()
	}()

	verifier := randomText(48) // 64 characters
	sum := sha256.Sum256([]byte(verifier))
	state := randomText(32)
	resource := base + kind.path()
	address, err := url.Parse(auth.AuthorizationEndpoint)
	if err != nil {
		return nil, nil, err
	}
	q := address.Query()
	for k, v := range map[string]string{
		"response_type": "code", "client_id": clientID, "redirect_uri": redirectURI, "state": state,
		"code_challenge": base64.RawURLEncoding.EncodeToString(sum[:]), "code_challenge_method": "S256", "resource": resource,
	} {
		q.Set(k, v)
	}
	address.RawQuery = q.Encode()

	timeout := a.LoginTimeout
	if timeout <= 0 {
		timeout = loginTimeout
	}
	var pasted chan string
	if noBrowser {
		fmt.Fprintf(a.Err, "Open this address in a browser, sign in and allow access:\n\n  %s\n\n", address)
		fmt.Fprintf(a.Err, "If the browser is on another computer, paste here the address it ends on (it starts with %s):\n", redirectURI)
		pasted = make(chan string)
		finished := make(chan struct{})
		defer close(finished)
		go func() {
			defer close(pasted)
			scanner := bufio.NewScanner(a.In)
			for scanner.Scan() {
				if line := strings.TrimSpace(scanner.Text()); line != "" {
					select {
					case pasted <- line:
					case <-finished:
						return
					}
				}
			}
		}()
	} else {
		fmt.Fprintf(a.Err, "Opening the sign-in page in your browser. If it does not open, open this address:\n\n  %s\n\n", address)
		if a.OpenBrowser == nil || a.OpenBrowser(address.String()) != nil {
			fmt.Fprintln(a.Err, "(The browser could not be opened: open the address yourself, or use --no-browser.)")
		}
		fmt.Fprintf(a.Err, "Waiting for you to sign in (up to %v)...\n", timeout)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var answer url.Values
	var reply chan bool
	for answer == nil {
		select {
		case cb := <-callbacks:
			answer, reply = cb.query, cb.reply
		case line, ok := <-pasted:
			if !ok {
				pasted = nil // standard input ended: the browser may still come back
				continue
			}
			u, err := url.Parse(line)
			if err != nil || u.Scheme != "http" || u.Host != host || u.Path != "/callback" || u.RawQuery == "" {
				fmt.Fprintf(a.Err, "That is not the address the browser ended on: paste the whole address, which starts with %s\n", redirectURI)
				continue
			}
			answer = u.Query()
		case <-timer.C:
			return nil, nil, &cliError{msg: fmt.Sprintf("the sign-in was not finished within %v", timeout), exit: exitError,
				hint: "Run `" + kind.loginCommand() + "` again."}
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	cred, err := a.finishLogin(ctx, auth, answer, state, clientID, verifier, redirectURI, resource)
	if reply != nil {
		reply <- err == nil
	}
	return cred, auth, err
}

// single is a parameter's one value; "" when it is missing or repeated.
func single(q url.Values, name string) string {
	if v := q[name]; len(v) == 1 {
		return v[0]
	}
	return ""
}

// finishLogin checks where the browser came back from (the state this sign-in made, compared in constant time, and
// the issuer, RFC 9207), then exchanges the code for tokens.
func (a *App) finishLogin(ctx context.Context, auth *authServer, q url.Values, state, clientID, verifier, redirectURI,
	resource string) (*OAuthCredential, error) {
	got := single(q, "state")
	if subtle.ConstantTimeCompare([]byte(got), []byte(state)) != 1 {
		return nil, &cliError{msg: "the answer's state is not this sign-in's: refusing it", exit: exitError,
			hint: "Sign in again, from the address this run prints."}
	}
	if single(q, "iss") != auth.Issuer {
		return nil, &cliError{msg: "the answer's iss is not " + auth.Issuer + ": it did not come from this sign-in's server; refusing it",
			exit: exitError}
	}
	if e := single(q, "error"); e != "" {
		msg := "the sign-in was refused: " + clean(e)
		if e == "access_denied" {
			msg = "the sign-in was cancelled (access_denied)"
		}
		return nil, &cliError{msg: msg, exit: exitError}
	}
	code := single(q, "code")
	if code == "" {
		return nil, &cliError{msg: "the answer has no code", exit: exitError}
	}
	t, err := auth.token(ctx, map[string]string{
		"grant_type": "authorization_code", "code": code, "code_verifier": verifier, "client_id": clientID,
		"redirect_uri": redirectURI, "resource": resource,
	})
	if err != nil {
		return nil, oauthFailure("the sign-in could not be finished", err)
	}
	return &OAuthCredential{ClientID: clientID, AccessToken: t.AccessToken, RefreshToken: t.RefreshToken,
		ExpiresAt: time.Now().Add(t.lifetime())}, nil
}

// openBrowser opens an address in the default browser.
func openBrowser(address string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", address)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", address)
	default:
		cmd = exec.Command("xdg-open", address)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
