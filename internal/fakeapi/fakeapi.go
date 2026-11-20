// Package fakeapi is an in-memory VerifyInboxes API for the tool's tests: the /v1 routes the tool calls, the
// sign-in endpoints (an authorization page that sends the browser straight back with a code) and an /mcp endpoint
// that answers with the headers it was sent.
//
// An address's verdict is told by how it starts: bad... is invalid, catch... catch_all (not safe to send), risky...
// risky, unknown... unknown; anything else is valid. A check of broke... is refused 402, as when the balance runs out.
package fakeapi

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Key is the API key the fake takes. BadKey is one it refuses.
const (
	Key    = "bv_testtesttesttesttesttesttesttest"
	BadKey = "bv_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	// Organisation is the name of the account behind every credential.
	Organisation = "Acme"
	// FailingDomain is the domain whose lookup answers 502 with an HTML page, as a proxy would.
	FailingDomain = "fail502.example"
)

// Options change how the fake answers. They are fixed when the fake starts.
type Options struct {
	// ListsStayRunning keeps every list running: a wait for one never ends.
	ListsStayRunning bool
	// Issuer is the issuer the metadata names; default the fake's own address.
	Issuer string
	// WrongState and WrongIssuer make the authorization page send the browser back with another state, or another
	// iss, than the request's.
	WrongState, WrongIssuer bool
	// AuthorizeError sends the browser back with this error instead of a code.
	AuthorizeError string
	// TokenLifetime is how long an access token works; default an hour.
	TokenLifetime time.Duration
	// OnFirstRefresh runs once, at the start of the first refresh grant the fake is asked for, before it is
	// answered.
	OnFirstRefresh func()
}

// Request is one request the fake answered.
type Request struct {
	Method, Path, Query string
	// Authorization is the bearer credential sent, without "Bearer ".
	Authorization string
	UserAgent     string
	// Header is the request's headers.
	Header http.Header
	// Body is the request's body (a form, JSON or a CSV upload).
	Body []byte
	// Status and RequestID are the answer's.
	Status    int
	RequestID string
}

// Server is the fake API, listening on 127.0.0.1.
type Server struct {
	*httptest.Server
	opts Options

	mu          sync.Mutex
	seen        []Request
	nextID      int
	jobs        map[string]*job
	jobOrder    []string
	webhooks    []map[string]any
	suppressed  []string
	addresses   int
	clients     map[string][]string
	codes       map[string]*code
	access      map[string]*token
	refresh     map[string]*token
	refreshDone bool
	refreshes   int
	refused     int
	revoked     []string
}

type job struct {
	id, kind, status string
	createdAt        string
	results          []map[string]any
	header           []string
	rows             [][]string
	emailColumn      int
}

type code struct {
	clientID, redirectURI, challenge, resource string
	used                                       bool
}

type token struct {
	clientID, resource string
	expires            time.Time
	used, revoked      bool
	grant              int
}

// New starts a fake API. Close it when done.
func New(opts Options) *Server {
	if opts.TokenLifetime == 0 {
		opts.TokenLifetime = time.Hour
	}
	s := &Server{
		opts: opts, jobs: map[string]*job{}, clients: map[string][]string{}, codes: map[string]*code{},
		access: map[string]*token{}, refresh: map[string]*token{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Seen returns every request answered so far, oldest first.
func (s *Server) Seen() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.seen...)
}

// Count returns how many requests had this method and path.
func (s *Server) Count(method, path string) int {
	n := 0
	for _, r := range s.Seen() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

// Refreshes returns how many refresh grants were asked for, and how many of them were refused.
func (s *Server) Refreshes() (asked, refused int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshes, s.refused
}

// Revoked returns the tokens revoked, in order.
func (s *Server) Revoked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.revoked...)
}

// ExpireAccessTokens makes every access token issued so far stop working, as an hour passing would, without the
// tool knowing.
func (s *Server) ExpireAccessTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.access {
		t.expires = time.Now().Add(-time.Minute)
	}
}

// LiveGrants returns how many sign-ins still work (a refresh token not used and not revoked).
func (s *Server) LiveGrants() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.refresh {
		if !t.used && !t.revoked {
			n++
		}
	}
	return n
}

// ---- dispatch --------------------------------------------------------------------------------------------------

type answer struct {
	status      int
	contentType string
	body        []byte
	header      http.Header
	redirect    string
	delay       time.Duration
}

func jsonAnswer(status int, v any) answer {
	body, _ := json.Marshal(v)
	return answer{status: status, contentType: "application/json", body: body}
}

func apiError(status int, code, message string) answer {
	return jsonAnswer(status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

func oauthError(status int, code, description string) answer {
	return jsonAnswer(status, map[string]any{"error": code, "error_description": description})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.nextID++
	requestID := fmt.Sprintf("req-%04d", s.nextID)
	s.mu.Unlock()

	a := s.route(r, body)
	if a.delay > 0 {
		time.Sleep(a.delay)
	}
	if a.header != nil {
		for k, v := range a.header {
			w.Header()[k] = v
		}
	}
	w.Header().Set("X-Request-Id", requestID)
	s.mu.Lock()
	s.seen = append(s.seen, Request{
		Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery,
		Authorization: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), UserAgent: r.UserAgent(),
		Header: r.Header.Clone(), Body: body, Status: a.status, RequestID: requestID,
	})
	s.mu.Unlock()
	if a.redirect != "" {
		http.Redirect(w, r, a.redirect, http.StatusFound)
		return
	}
	if a.contentType != "" {
		w.Header().Set("Content-Type", a.contentType)
	}
	w.WriteHeader(a.status)
	_, _ = w.Write(a.body)
}

func (s *Server) route(r *http.Request, body []byte) answer {
	path := r.URL.Path
	switch {
	case path == "/.well-known/oauth-authorization-server" && r.Method == http.MethodGet:
		return s.metadata()
	case path == "/oauth/register" && r.Method == http.MethodPost:
		return s.register(body)
	case path == "/oauth/authorize" && r.Method == http.MethodGet:
		return s.authorize(r.URL.Query())
	case path == "/oauth/token" && r.Method == http.MethodPost:
		return s.token(body)
	case path == "/oauth/revoke" && r.Method == http.MethodPost:
		return s.revoke(body)
	case path == "/mcp":
		return s.mcp(r, body)
	case strings.HasPrefix(path, "/v1/"):
		if a, ok := s.authenticate(r, "/v1"); !ok {
			return a
		}
		return s.v1(r, body)
	}
	return apiError(http.StatusNotFound, "not_found", "no such route")
}

// authenticate checks the bearer credential for the resource (/v1 or /mcp); the answer is the refusal.
func (s *Server) authenticate(r *http.Request, resource string) (answer, bool) {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if bearer == Key {
		return answer{}, true
	}
	s.mu.Lock()
	t, ok := s.access[bearer]
	live := ok && !t.revoked && time.Now().Before(t.expires) && t.resource == s.URL+resource
	s.mu.Unlock()
	if live {
		return answer{}, true
	}
	a := apiError(http.StatusUnauthorized, "unauthorized", "the API key or access token is not valid")
	if strings.HasPrefix(bearer, "vi_at_") {
		a.header = http.Header{"Www-Authenticate": {`Bearer error="invalid_token"`}}
	}
	return a, false
}

// ---- sign-in ---------------------------------------------------------------------------------------------------

func (s *Server) issuer() string {
	if s.opts.Issuer != "" {
		return s.opts.Issuer
	}
	return s.URL
}

func (s *Server) metadata() answer {
	return jsonAnswer(http.StatusOK, map[string]any{
		"issuer":                                         s.issuer(),
		"authorization_endpoint":                         s.URL + "/oauth/authorize",
		"token_endpoint":                                 s.URL + "/oauth/token",
		"registration_endpoint":                          s.URL + "/oauth/register",
		"revocation_endpoint":                            s.URL + "/oauth/revoke",
		"scopes_supported":                               []string{"api"},
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"authorization_response_iss_parameter_supported": true,
	})
}

func (s *Server) register(body []byte) answer {
	var meta struct {
		RedirectURIs []string `json:"redirect_uris"`
	}
	if json.Unmarshal(body, &meta) != nil || len(meta.RedirectURIs) == 0 {
		return oauthError(http.StatusBadRequest, "invalid_client_metadata", "redirect_uris is required")
	}
	id := "vi_cl_" + randomText(24)
	s.mu.Lock()
	s.clients[id] = meta.RedirectURIs
	s.mu.Unlock()
	return jsonAnswer(http.StatusCreated, map[string]any{
		"client_id": id, "redirect_uris": meta.RedirectURIs, "token_endpoint_auth_method": "none",
	})
}

var loopback = regexp.MustCompile(`^http://127\.0\.0\.1(?::\d+)?(/.*)$`)

// redirectMatches is the API's rule: equal, but for the port of a loopback IP.
func redirectMatches(registered, requested string) bool {
	if registered == requested {
		return true
	}
	r, q := loopback.FindStringSubmatch(registered), loopback.FindStringSubmatch(requested)
	return r != nil && q != nil && r[1] == q[1]
}

func (s *Server) authorize(q url.Values) answer {
	s.mu.Lock()
	registered, ok := s.clients[q.Get("client_id")]
	s.mu.Unlock()
	if !ok {
		return apiError(http.StatusBadRequest, "invalid_client", "this app is not registered")
	}
	redirect := q.Get("redirect_uri")
	matched := false
	for _, r := range registered {
		matched = matched || redirectMatches(r, redirect)
	}
	if !matched {
		return apiError(http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri is not one the app registered")
	}
	back := url.Values{}
	resource := q.Get("resource")
	if resource == "" {
		resource = s.URL + "/mcp"
	}
	switch {
	case s.opts.AuthorizeError != "":
		back.Set("error", s.opts.AuthorizeError)
	case q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43:
		back.Set("error", "invalid_request")
	case resource != s.URL+"/v1" && resource != s.URL+"/mcp":
		back.Set("error", "invalid_target")
	default:
		c := "vi_ac_" + randomText(40)
		s.mu.Lock()
		s.codes[c] = &code{clientID: q.Get("client_id"), redirectURI: redirect, challenge: q.Get("code_challenge"), resource: resource}
		s.mu.Unlock()
		back.Set("code", c)
	}
	state := q.Get("state")
	if s.opts.WrongState {
		state = "not-the-state-that-was-sent"
	}
	back.Set("state", state)
	iss := s.URL
	if s.opts.WrongIssuer {
		iss = "https://elsewhere.example"
	}
	back.Set("iss", iss)
	return answer{redirect: redirect + "?" + back.Encode()}
}

func (s *Server) issue(clientID, resource string) (map[string]any, error) {
	access, refresh := "vi_at_"+randomText(40), "vi_rt_"+randomText(40)
	s.access[access] = &token{clientID: clientID, resource: resource, expires: time.Now().Add(s.opts.TokenLifetime)}
	s.refresh[refresh] = &token{clientID: clientID, resource: resource, expires: time.Now().Add(30 * 24 * time.Hour)}
	return map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": int(s.opts.TokenLifetime / time.Second),
		"refresh_token": refresh, "scope": "api",
	}, nil
}

func (s *Server) token(body []byte) answer {
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return oauthError(http.StatusBadRequest, "invalid_request", "the body must be a form")
	}
	switch form.Get("grant_type") {
	case "authorization_code":
		s.mu.Lock()
		defer s.mu.Unlock()
		c, ok := s.codes[form.Get("code")]
		if !ok || c.used || c.clientID != form.Get("client_id") || c.redirectURI != form.Get("redirect_uri") {
			return oauthError(http.StatusBadRequest, "invalid_grant", "the code is not valid")
		}
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
			return oauthError(http.StatusBadRequest, "invalid_grant", "the code_verifier does not match")
		}
		if r := form.Get("resource"); r != "" && r != c.resource {
			return oauthError(http.StatusBadRequest, "invalid_target", "resource is not the one asked for")
		}
		c.used = true
		tokens, _ := s.issue(c.clientID, c.resource)
		return jsonAnswer(http.StatusOK, tokens)
	case "refresh_token":
		s.mu.Lock()
		hook := s.opts.OnFirstRefresh
		first := !s.refreshDone
		s.refreshDone = true
		s.refreshes++
		s.mu.Unlock()
		if first && hook != nil {
			hook()
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		t, ok := s.refresh[form.Get("refresh_token")]
		if !ok || t.used || t.revoked || t.clientID != form.Get("client_id") {
			s.refused++
			return oauthError(http.StatusBadRequest, "invalid_grant", "the refresh token is not valid")
		}
		if r := form.Get("resource"); r != "" && r != t.resource {
			s.refused++
			return oauthError(http.StatusBadRequest, "invalid_target", "resource is not the one access was granted for")
		}
		t.used = true
		tokens, _ := s.issue(t.clientID, t.resource)
		return jsonAnswer(http.StatusOK, tokens)
	}
	return oauthError(http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
}

func (s *Server) revoke(body []byte) answer {
	form, _ := url.ParseQuery(string(body))
	s.mu.Lock()
	defer s.mu.Unlock()
	given := form.Get("token")
	if t, ok := s.refresh[given]; ok && t.clientID == form.Get("client_id") {
		t.revoked = true
		for _, a := range s.access {
			if a.clientID == t.clientID {
				a.revoked = true
			}
		}
	}
	s.revoked = append(s.revoked, given)
	return answer{status: http.StatusOK}
}

// ---- /mcp ------------------------------------------------------------------------------------------------------

func (s *Server) mcp(r *http.Request, body []byte) answer {
	if r.Method != http.MethodPost {
		return apiError(http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
	}
	if r.Header.Get("Origin") != "" {
		return apiError(http.StatusForbidden, "forbidden", "browser requests are not accepted")
	}
	if a, ok := s.authenticate(r, "/mcp"); !ok {
		return a
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name            string `json:"name"`
			ProtocolVersion string `json:"protocolVersion"`
			DelayMS         int    `json:"delay_ms"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return jsonAnswer(http.StatusBadRequest, map[string]any{
			"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "Parse error"},
		})
	}
	if len(msg.ID) == 0 {
		return answer{status: http.StatusAccepted}
	}
	headers := map[string]string{}
	for _, name := range []string{"mcp-protocol-version", "mcp-method", "mcp-name", "content-type", "accept", "user-agent"} {
		if v := r.Header.Get(name); v != "" {
			headers[name] = v
		}
	}
	result := map[string]any{"method": msg.Method, "headers": headers}
	delay := time.Duration(msg.Params.DelayMS) * time.Millisecond
	switch msg.Method {
	case "initialize":
		version := msg.Params.ProtocolVersion
		if version == "" {
			version = "2025-11-25"
		}
		result["protocolVersion"] = version
		result["serverInfo"] = map[string]any{"name": "fake", "version": "1"}
		result["capabilities"] = map[string]any{"tools": map[string]any{}}
	case "boom":
		return apiError(http.StatusInternalServerError, "internal", "something broke")
	case "stream_unanswered":
		// A stream that ends after a notification, without the answer.
		note, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{"progress": 1}})
		return answer{status: http.StatusOK, contentType: "text/event-stream", body: []byte("event: message\ndata: " + string(note) + "\n\n")}
	case "stream":
		note, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{"progress": 1}})
		done, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		sse := "event: message\ndata: " + string(note) + "\n\n" + "event: message\ndata: " + string(done) + "\n\n"
		return answer{status: http.StatusOK, contentType: "text/event-stream", body: []byte(sse), delay: delay}
	}
	a := jsonAnswer(http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	// Indented, as a server may send it: the tool must still write it as one line.
	var pretty bytes.Buffer
	_ = json.Indent(&pretty, a.body, "", "  ")
	a.body, a.delay = pretty.Bytes(), delay
	return a
}

// ---- /v1 -------------------------------------------------------------------------------------------------------

func verdictOf(email string) map[string]any {
	local := strings.ToLower(email)
	if i := strings.IndexByte(local, '@'); i >= 0 {
		local = local[:i]
	}
	r := map[string]any{
		"email": email, "verdict": "valid", "score": nil, "safe_to_send": true, "flags": []string{},
		"provider": "google", "catch_all_domain": false, "verified_at": "2026-11-18T12:00:00Z", "source": "smtp",
		"reason": "mailbox_exists",
	}
	switch {
	case strings.HasPrefix(local, "bad"):
		r["verdict"], r["safe_to_send"], r["reason"] = "invalid", false, "mailbox_not_found"
	case strings.HasPrefix(local, "catch"):
		r["verdict"], r["safe_to_send"], r["reason"], r["score"], r["catch_all_domain"] = "catch_all", false, "accept_all", 0.4, true
	case strings.HasPrefix(local, "risky"):
		r["verdict"], r["safe_to_send"], r["reason"] = "risky", false, "greylisted"
	case strings.HasPrefix(local, "unknown"):
		r["verdict"], r["safe_to_send"], r["reason"] = "unknown", false, "blocked"
	}
	return r
}

func (s *Server) newJob(kind string) *job {
	j := &job{id: fmt.Sprintf("019a0000-0000-7000-8000-%012d", len(s.jobOrder)+1), kind: kind, status: "done",
		createdAt: "2026-11-18T12:00:00Z"}
	s.jobs[j.id] = j
	s.jobOrder = append(s.jobOrder, j.id)
	return j
}

func (j *job) view() map[string]any {
	v := map[string]any{
		"id": j.id, "kind": j.kind, "status": j.status, "total": len(j.results), "done": len(j.results),
		"created_at": j.createdAt, "finished_at": nil, "counts": nil, "purged": false,
	}
	if j.status == "running" {
		v["done"] = 0
		return v
	}
	v["finished_at"] = "2026-11-18T12:01:00Z"
	counts := map[string]int{"valid": 0, "invalid": 0, "catch_all": 0, "risky": 0, "unknown": 0}
	for _, r := range j.results {
		counts[r["verdict"].(string)]++
	}
	v["counts"] = counts
	if j.kind == "single" {
		result := map[string]any{}
		for k, val := range j.results[0] {
			result[k] = val
		}
		result["job_id"] = j.id
		v["result"] = result
	}
	return v
}

var jobPath = regexp.MustCompile(`^/v1/jobs/([^/]+)(/results|/download|/resolve)?$`)

func (s *Server) v1(r *http.Request, body []byte) answer {
	path := r.URL.Path
	switch {
	case path == "/v1/account" && r.Method == http.MethodGet:
		return jsonAnswer(http.StatusOK, map[string]any{
			"name": Organisation, "balance": 1000, "reviewed": true, "test_emails": false, "safe_to_send_threshold": 0.7,
		})
	case path == "/v1/verify" && r.Method == http.MethodPost:
		var in struct {
			Email string `json:"email"`
		}
		if json.Unmarshal(body, &in) != nil || in.Email == "" {
			return apiError(http.StatusBadRequest, "invalid_request", "email is required")
		}
		if strings.HasPrefix(in.Email, "broke") {
			return apiError(http.StatusPaymentRequired, "insufficient_credits", "the balance does not cover this check")
		}
		s.mu.Lock()
		j := s.newJob("single")
		j.results = []map[string]any{verdictOf(in.Email)}
		s.mu.Unlock()
		out := verdictOf(in.Email)
		out["job_id"] = j.id
		return jsonAnswer(http.StatusOK, out)
	case path == "/v1/jobs" && r.Method == http.MethodPost:
		return s.createList(r, body)
	case path == "/v1/suppressions" && r.Method == http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		return jsonAnswer(http.StatusOK, map[string]any{"domains": append([]string{}, s.suppressed...), "address_count": s.addresses})
	case path == "/v1/suppressions" && r.Method == http.MethodPost:
		var in struct {
			Emails  []string `json:"emails"`
			Domains []string `json:"domains"`
		}
		_ = json.Unmarshal(body, &in)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.addresses += len(in.Emails)
		s.suppressed = append(s.suppressed, in.Domains...)
		return jsonAnswer(http.StatusOK, map[string]any{"added": len(in.Emails) + len(in.Domains)})
	case path == "/v1/webhooks" && r.Method == http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		return jsonAnswer(http.StatusOK, append([]map[string]any{}, s.webhooks...))
	case path == "/v1/webhooks" && r.Method == http.MethodPost:
		var in struct {
			URL    string   `json:"url"`
			Events []string `json:"events"`
		}
		if json.Unmarshal(body, &in) != nil || in.URL == "" {
			return apiError(http.StatusBadRequest, "invalid_request", "url is required")
		}
		if len(in.Events) == 0 {
			in.Events = []string{"job.completed"}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		hook := map[string]any{"id": fmt.Sprintf("019a0000-0000-7000-9000-%012d", len(s.webhooks)+1), "url": in.URL,
			"events": in.Events, "created_at": "2026-11-18T12:00:00Z"}
		s.webhooks = append(s.webhooks, hook)
		created := map[string]any{"secret": strings.Repeat("ab", 32)}
		for k, v := range hook {
			created[k] = v
		}
		return jsonAnswer(http.StatusCreated, created)
	case strings.HasPrefix(path, "/v1/webhooks/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(path, "/v1/webhooks/")
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, h := range s.webhooks {
			if h["id"] == id {
				s.webhooks = append(s.webhooks[:i], s.webhooks[i+1:]...)
				return answer{status: http.StatusNoContent}
			}
		}
		return apiError(http.StatusNotFound, "not_found", "webhook not found")
	case strings.HasPrefix(path, "/v1/domains/") && r.Method == http.MethodGet:
		domain := strings.TrimPrefix(path, "/v1/domains/")
		if domain == FailingDomain {
			return answer{status: http.StatusBadGateway, contentType: "text/html", body: []byte("<html>Bad gateway</html>")}
		}
		return jsonAnswer(http.StatusOK, map[string]any{
			"domain": domain, "provider": "google", "gateway": false, "accepts_mail": true,
			"mx_hosts":  []map[string]any{{"host": "aspmx.l.google.com", "preference": 1}},
			"catch_all": false, "catch_all_checked_at": nil, "email_pattern": "first.last", "pattern_confidence": 0.8,
			"registered_at": nil,
		})
	}
	if m := jobPath.FindStringSubmatch(path); m != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		j, ok := s.jobs[m[1]]
		if !ok {
			return apiError(http.StatusNotFound, "not_found", "job not found")
		}
		switch {
		case m[2] == "" && r.Method == http.MethodGet:
			return jsonAnswer(http.StatusOK, j.view())
		case m[2] == "/results" && r.Method == http.MethodGet:
			return s.results(j, r.URL.Query())
		case m[2] == "/download" && r.Method == http.MethodGet:
			return s.download(j, r.URL.Query())
		case m[2] == "/resolve" && r.Method == http.MethodPost:
			res := s.newJob("resolve")
			return jsonAnswer(http.StatusCreated, map[string]any{"id": res.id, "status": "running", "total": 0,
				"skipped": map[string]int{"ineligible": len(j.results), "excluded": 0, "already_requested": 0, "duplicate": 0}})
		}
	}
	return apiError(http.StatusNotFound, "not_found", "no such route")
}

func (s *Server) createList(r *http.Request, body []byte) answer {
	var emails []string
	var header []string
	var rows [][]string
	column := 0
	mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch mediaType {
	case "application/json":
		var in struct {
			Emails []string `json:"emails"`
		}
		if json.Unmarshal(body, &in) != nil {
			return apiError(http.StatusBadRequest, "invalid_request", "the body is not JSON")
		}
		emails, header = in.Emails, []string{"email"}
		for _, e := range emails {
			rows = append(rows, []string{e})
		}
	case "multipart/form-data":
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		emailColumn := "email"
		for {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(part)
			switch part.FormName() {
			case "email_column":
				emailColumn = string(data)
			case "file":
				records, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
				if err != nil || len(records) == 0 {
					return apiError(http.StatusBadRequest, "invalid_request", "the file is not a CSV")
				}
				header, rows = records[0], records[1:]
			}
		}
		column = -1
		for i, h := range header {
			if h == emailColumn {
				column = i
			}
		}
		if column < 0 {
			return apiError(http.StatusUnprocessableEntity, "invalid_request", "the CSV has no column "+emailColumn)
		}
		for _, row := range rows {
			emails = append(emails, row[column])
		}
	default:
		return apiError(http.StatusUnsupportedMediaType, "invalid_request", "send JSON or a CSV")
	}
	if len(emails) == 0 {
		return apiError(http.StatusUnprocessableEntity, "empty_list", "the list has no addresses")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.newJob("bulk")
	j.header, j.rows, j.emailColumn = header, rows, column
	for _, e := range emails {
		j.results = append(j.results, verdictOf(e))
	}
	if s.opts.ListsStayRunning {
		j.status = "running"
	}
	return jsonAnswer(http.StatusCreated, map[string]any{"id": j.id, "status": "running", "total": len(emails)})
}

// keep reports whether a result passes the results' or the download's filters.
func keep(result map[string]any, q url.Values) bool {
	if q.Get("only") == "safe_to_send" && result["safe_to_send"] != true {
		return false
	}
	if v := q.Get("verdicts"); v != "" {
		for _, want := range strings.Split(v, ",") {
			if result["verdict"] == want {
				return true
			}
		}
		return false
	}
	return true
}

func (s *Server) results(j *job, q url.Values) answer {
	if j.status == "running" && q.Get("partial") != "1" {
		return apiError(http.StatusConflict, "not_ready", "the list has not finished")
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 500
	}
	start, _ := strconv.Atoi(q.Get("cursor"))
	data := []map[string]any{}
	i := start
	for ; i < len(j.results) && len(data) < limit; i++ {
		if !keep(j.results[i], q) {
			continue
		}
		row := map[string]any{"row": i + 1}
		for k, v := range j.results[i] {
			row[k] = v
		}
		data = append(data, row)
	}
	var next any
	if i < len(j.results) {
		next = strconv.Itoa(i)
	}
	return jsonAnswer(http.StatusOK, map[string]any{"data": data, "next_cursor": next, "complete": j.status == "done"})
}

func (s *Server) download(j *job, q url.Values) answer {
	if j.status == "running" && q.Get("partial") != "1" {
		return apiError(http.StatusConflict, "not_ready", "the list has not finished")
	}
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	_ = w.Write(append(append([]string{}, j.header...), "verdict", "safe_to_send", "reason"))
	for i, row := range j.rows {
		r := j.results[i]
		if !keep(r, q) {
			continue
		}
		_ = w.Write(append(append([]string{}, row...), r["verdict"].(string), fmt.Sprint(r["safe_to_send"]), r["reason"].(string)))
	}
	w.Flush()
	return answer{status: http.StatusOK, contentType: "text/csv", body: b.Bytes()}
}

// randomText is n random letters and digits.
func randomText(n int) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, n)
	for i := range out {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			panic(err)
		}
		out[i] = chars[k.Int64()]
	}
	return string(out)
}
