package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/verifyinboxes/verifyinboxes-go"
)

func (a *App) cmdMcp() *cobra.Command {
	return &cobra.Command{
		Use:     "mcp",
		GroupID: "setup",
		Short:   "Serve the VerifyInboxes MCP server to an AI app over stdio",
		Long: `Give an AI app that starts MCP servers as programs (stdio) the VerifyInboxes MCP server: this command is a
bridge to it, so the tools are the server's own, word for word. Each JSON-RPC message read on standard input is
sent to <base URL>/mcp, and each answer written to standard output, one per line; nothing else is written there
(notes and errors go to standard error). It ends when standard input closes.

It uses VERIFYINBOXES_API_KEY, else a saved API key, else the sign-in "verifyinboxes login --for mcp" made, which
acts as you, with your role.`,
		Example: `  verifyinboxes login --for mcp
  verifyinboxes mcp`,
		Args: exactArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			base, err := a.baseURL()
			if err != nil {
				return err
			}
			cred, err := a.mcpCredential(base)
			if err != nil {
				return err
			}
			b := &bridge{app: a, endpoint: base + "/mcp", cred: cred, userAgent: a.userAgent() + " verifyinboxes-go/" + verifyinboxes.Version,
				http: &http.Client{Timeout: 5 * time.Minute,
					CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
			return b.run(cmd.Context())
		},
	}
}

// staticKey is an API key as a token source.
type staticKey string

func (k staticKey) Token(context.Context) (string, error) { return string(k), nil }

// mcpCredential is VERIFYINBOXES_API_KEY, else the saved key, else the sign-in for AI apps.
func (a *App) mcpCredential(base string) (verifyinboxes.TokenSource, error) {
	key := envKey()
	if key == "" {
		store, err := a.store()
		if err != nil {
			return nil, err
		}
		saved, err := store.load(base)
		if err != nil {
			return nil, err
		}
		switch {
		case saved.APIKey != "":
			key = saved.APIKey
		case saved.MCP != nil:
			return a.signIn(base, forMCP, *saved.MCP), nil
		default:
			return nil, &cliError{msg: `not signed in for AI apps: run "verifyinboxes login --for mcp" once, or set ` + envAPIKey,
				exit: exitError}
		}
	}
	if !visible(key) {
		return nil, &cliError{msg: "the API key holds a space, a control character or a character that is not ASCII: copy it again, whole",
			exit: exitError}
	}
	return staticKey(key), nil
}

// bridge relays newline-delimited JSON-RPC between standard input and output and the MCP server's endpoint.
type bridge struct {
	app       *App
	endpoint  string
	cred      verifyinboxes.TokenSource
	userAgent string
	http      *http.Client

	outMu sync.Mutex

	versionMu sync.Mutex
	version   string // what initialize negotiated

	refreshMu sync.Mutex
}

// message is what the bridge reads of a JSON-RPC message.
type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name string                     `json:"name"`
		Meta map[string]json.RawMessage `json:"_meta"`
	} `json:"params"`
}

func (m *message) isRequest() bool {
	return m.Method != "" && len(m.ID) > 0 && string(m.ID) != "null"
}

func (b *bridge) run(ctx context.Context) error {
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		reader := bufio.NewReaderSize(b.app.In, 64*1024)
		for {
			line, err := reader.ReadBytes('\n')
			if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
				select {
				case lines <- trimmed:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				readErr <- err
				return
			}
		}
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case line := <-lines:
			wg.Add(1)
			go func() {
				defer wg.Done()
				b.relay(ctx, line)
			}()
		case err := <-readErr:
			wg.Wait()
			if err != nil {
				return &cliError{msg: "could not read standard input: " + err.Error(), exit: exitError}
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// write writes one message to standard output, as one line.
func (b *bridge) write(data []byte) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		b.log("the server sent something that is not JSON; it was dropped")
		return
	}
	compact.WriteByte('\n')
	b.outMu.Lock()
	defer b.outMu.Unlock()
	_, _ = b.app.Out.Write(compact.Bytes())
}

// log prints a line on standard error, cleaned whole: what it says may be the server's text.
func (b *bridge) log(format string, args ...any) {
	b.outMu.Lock()
	defer b.outMu.Unlock()
	fmt.Fprintln(b.app.Err, "verifyinboxes mcp: "+clean(fmt.Sprintf(format, args...)))
}

// relay sends one message and writes what comes back.
func (b *bridge) relay(ctx context.Context, line []byte) {
	var msg message
	if err := json.Unmarshal(line, &msg); err != nil {
		msg = message{} // not an object (a batch, or not JSON): sent as it is, and the server says what it thinks
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json, text/event-stream")
	headers.Set("User-Agent", b.userAgent)
	modern := false
	if raw, ok := msg.Params.Meta["io.modelcontextprotocol/protocolVersion"]; ok {
		var version string
		if json.Unmarshal(raw, &version) == nil && version != "" {
			modern = true
			headers.Set("Mcp-Protocol-Version", version)
			headers.Set("Mcp-Method", msg.Method)
			if msg.Method == "tools/call" && msg.Params.Name != "" {
				headers.Set("Mcp-Name", msg.Params.Name)
			}
		}
	}
	if !modern {
		b.versionMu.Lock()
		if b.version != "" {
			headers.Set("Mcp-Protocol-Version", b.version)
		}
		b.versionMu.Unlock()
	}

	resp, err := b.send(ctx, line, headers)
	if err != nil {
		b.fail(&msg, err.Error(), nil)
		return
	}
	defer resp.Body.Close()
	contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case resp.StatusCode == http.StatusAccepted:
		_, _ = io.Copy(io.Discard, resp.Body)
	case resp.StatusCode >= 200 && resp.StatusCode <= 299 && contentType == "text/event-stream":
		answered, err := b.events(resp.Body, msg.ID)
		if !answered {
			// The request is answered all the same: an app waiting for its reply would otherwise wait for ever.
			why := "the MCP server's event stream ended without an answer"
			if err != nil {
				why = "the MCP server's event stream was cut off before its answer: " + err.Error()
			}
			b.fail(&msg, why, resp)
		}
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			b.fail(&msg, "the answer was cut off: "+err.Error(), resp)
			return
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return
		}
		if msg.Method == "initialize" && !modern {
			b.negotiated(data)
		}
		b.write(data)
	default:
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		var rpc struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if json.Unmarshal(data, &rpc) == nil && rpc.JSONRPC != "" {
			b.write(data) // the protocol's own answer to a request it refused
			return
		}
		message := fmt.Sprintf("the MCP server answered %d", resp.StatusCode)
		var api struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &api) == nil && api.Error.Message != "" {
			message = api.Error.Message + " (" + api.Error.Code + ", HTTP " + fmt.Sprint(resp.StatusCode) + ")"
		}
		b.fail(&msg, message, resp)
	}
}

// send POSTs the message with the credential; a 401 renews a sign-in once and sends it again.
func (b *bridge) send(ctx context.Context, body []byte, headers http.Header) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		token, err := b.cred.Token(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = headers.Clone()
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := b.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("no answer from %s: %s", b.endpoint, causeOf(err))
		}
		refresher, ok := b.cred.(verifyinboxes.Refresher)
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 || !ok {
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if err := b.refresh(ctx, refresher, token); err != nil {
			return nil, err
		}
	}
}

// refresh renews the refused token once for all the messages refused with it.
func (b *bridge) refresh(ctx context.Context, refresher verifyinboxes.Refresher, refused string) error {
	b.refreshMu.Lock()
	defer b.refreshMu.Unlock()
	if current, err := b.cred.Token(ctx); err == nil && current != refused {
		return nil
	}
	return refresher.Refresh(ctx)
}

// negotiated keeps the protocol version an initialize answer names.
func (b *bridge) negotiated(data []byte) {
	var answer struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if json.Unmarshal(data, &answer) == nil && visible(answer.Result.ProtocolVersion) {
		b.versionMu.Lock()
		b.version = answer.Result.ProtocolVersion
		b.versionMu.Unlock()
	}
}

// events writes an event stream's messages, one line each, and reports whether one of them answered the request
// whose id is id (and, when the stream did not end cleanly, why).
func (b *bridge) events(body io.Reader, id json.RawMessage) (answered bool, err error) {
	reader := bufio.NewReader(body)
	var data []string
	event := ""
	dispatch := func() {
		if len(data) > 0 && (event == "" || event == "message") {
			message := []byte(strings.Join(data, "\n"))
			answered = answered || answers(message, id)
			b.write(message)
		}
		data, event = nil, ""
	}
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if err == nil {
				dispatch()
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if err != nil {
			dispatch()
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return answered, err
		}
	}
}

// answers reports whether message is the reply (a result or an error) to the request whose id is id.
func answers(message []byte, id json.RawMessage) bool {
	if len(id) == 0 {
		return false
	}
	var reply struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(message, &reply) != nil || (reply.Result == nil && reply.Error == nil) {
		return false
	}
	var got, want any
	return json.Unmarshal(reply.ID, &got) == nil && json.Unmarshal(id, &want) == nil && reflect.DeepEqual(got, want)
}

// fail answers a request that could not be relayed with a JSON-RPC error carrying what went wrong (the API's
// message and its request id); a notification's failure is only logged.
func (b *bridge) fail(msg *message, message string, resp *http.Response) {
	data := map[string]any{}
	if resp != nil {
		data["status"] = resp.StatusCode
		if id := resp.Header.Get("X-Request-Id"); id != "" {
			message += " (request id " + id + ")"
			data["request_id"] = id
		}
	}
	message = redact(message)
	if !msg.isRequest() {
		b.log("%s", message)
		return
	}
	answer, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32000, "message": message, "data": data},
	})
	b.write(answer)
}
