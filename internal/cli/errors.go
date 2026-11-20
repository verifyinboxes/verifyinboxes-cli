package cli

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/verifyinboxes/verifyinboxes-go"
)

// Exit codes.
const (
	exitOK          = 0
	exitError       = 1
	exitUsage       = 2
	exitFailOn      = 3
	exitInterrupted = 130
)

// cliError is an error with what a person needs: what happened, the API's code, status and request id, the jobs
// that carry on, and what to do next.
type cliError struct {
	msg       string
	code      string
	status    int
	requestID string
	hint      string
	exit      int
	jobIDs    []string
	// quiet: say nothing more than msg (no "error:" in front): --fail-on's exit 3.
	plain bool
}

func (e *cliError) Error() string { return e.msg }

func usageError(msg, hint string) *cliError { return &cliError{msg: msg, hint: hint, exit: exitUsage} }

// jobsError names the jobs a failed command had made, which carry on.
type jobsError struct {
	err    error
	jobIDs []string
}

func (e *jobsError) Error() string { return e.err.Error() }
func (e *jobsError) Unwrap() error { return e.err }

// withJobs names the jobs on err, so that they are never lost.
func withJobs(err error, ids ...string) error {
	if err == nil || len(ids) == 0 {
		return err
	}
	return &jobsError{err: err, jobIDs: ids}
}

// waitTimeout is a wait that ran out while the job was still running.
type waitTimeout struct {
	jobID  string
	waited time.Duration
}

func (e *waitTimeout) Error() string { return fmt.Sprintf("still running after %v", e.waited) }

// jobIDsOf is every job id err names: the SDK's, then the tool's own.
func jobIDsOf(err error) []string {
	ids := verifyinboxes.JobIDs(err)
	var own *jobsError
	if errors.As(err, &own) {
		ids = append(ids, own.jobIDs...)
	}
	var wait *waitTimeout
	if errors.As(err, &wait) {
		ids = append(ids, wait.jobID)
	}
	var seen = map[string]bool{}
	var out []string
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// classify turns any error into a cliError. The job ids come first, from whatever error it is: a wait that ran out,
// a refused request or an interrupt after a job was made all name it.
func (a *App) classify(ctx context.Context, err error) *cliError {
	e := a.describe(ctx, err)
	if len(e.jobIDs) == 0 {
		e.jobIDs = jobIDsOf(err)
	}
	var api *verifyinboxes.APIError
	if errors.As(err, &api) {
		if e.requestID == "" {
			e.requestID = api.RequestID
		}
		if e.status == 0 {
			e.status = api.StatusCode
		}
		if e.code == "" {
			e.code = api.Code
		}
	}
	return e
}

func (a *App) describe(ctx context.Context, err error) *cliError {
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, ctx.Err())) {
		return &cliError{msg: "interrupted", exit: exitInterrupted}
	}
	var own *cliError
	if errors.As(err, &own) {
		copied := *own
		return &copied
	}
	var wait *waitTimeout
	if errors.As(err, &wait) {
		return &cliError{msg: wait.Error(), exit: exitError}
	}
	var sdkWait *verifyinboxes.WaitTimeoutError
	if errors.As(err, &sdkWait) {
		return &cliError{msg: fmt.Sprintf("still running after %v", sdkWait.Waited), exit: exitError}
	}
	var api *verifyinboxes.APIError
	if errors.As(err, &api) {
		return a.apiFailure(api)
	}
	var conn *verifyinboxes.ConnectionError
	if errors.As(err, &conn) {
		base, _ := a.baseURL()
		e := &cliError{msg: strings.TrimPrefix(conn.Error(), "verifyinboxes: "), exit: exitError,
			hint: "Check your connection and " + base + ", then try again."}
		return e
	}
	if errors.Is(err, verifyinboxes.ErrMissingAPIKey) {
		return &cliError{msg: "not signed in", exit: exitError,
			hint: "Run `verifyinboxes login`, or set " + envAPIKey + "."}
	}
	if !a.started { // cobra refused the command line before any command ran
		return usageError(err.Error(), "See `verifyinboxes --help`.")
	}
	return &cliError{msg: strings.TrimPrefix(err.Error(), "verifyinboxes: "), exit: exitError}
}

func (a *App) apiFailure(api *verifyinboxes.APIError) *cliError {
	e := &cliError{msg: api.Message, code: api.Code, status: api.StatusCode, requestID: api.RequestID, exit: exitError}
	switch {
	case errors.Is(api, verifyinboxes.ErrAuthentication):
		e.hint = "Sign in again (`verifyinboxes login`), or check " + envAPIKey + " or the key you saved; an organisation " +
			"admin makes keys in the VerifyInboxes app, under Developers -> API keys."
	case errors.Is(api, verifyinboxes.ErrInsufficientCredits):
		e.hint = "Nothing was charged for this request. Buy credits in the VerifyInboxes app, then try again."
	case errors.Is(api, verifyinboxes.ErrPermissionDenied) && api.Code == "forbidden":
		e.hint = "This needs an organisation admin."
	case errors.Is(api, verifyinboxes.ErrRateLimited):
		if api.RetryAfter > 0 {
			e.hint = fmt.Sprintf("Try again in %v.", api.RetryAfter)
		}
	case errors.Is(api, verifyinboxes.ErrServer):
		e.hint = "Try again in a moment. If it keeps failing, write to support@verifyinboxes.com with the request id."
	}
	return e
}

// secretLike is a key, a token or a code: never printed, whatever said it.
var secretLike = regexp.MustCompile(`(bv|vi_at|vi_rt|vi_ac|vi_cs|whk)_[A-Za-z0-9]+`)

func redact(s string) string { return secretLike.ReplaceAllString(s, "[hidden]") }

// report prints an error on standard error and returns the exit code:
//
//	error: <message> (<code>, HTTP <status>, request id <id>)
//	hint: <what to do>
//
// and, when jobs were made, each job id and how to wait for it again.
func (a *App) report(ctx context.Context, err error) int {
	e := a.classify(ctx, err)
	if e.plain {
		if !a.quiet {
			fmt.Fprintln(a.Err, redact(clean(e.msg)))
		}
		return e.exit
	}
	var parts []string
	if e.code != "" {
		parts = append(parts, e.code)
	}
	if e.status != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.status))
	}
	if e.requestID != "" && !strings.Contains(e.msg, "request id "+e.requestID) {
		parts = append(parts, "request id "+e.requestID)
	}
	// Every line is cleaned whole: the code, the request id and the job ids are the API's text too.
	line := "error: " + e.msg
	if len(parts) > 0 {
		line += " (" + strings.Join(parts, ", ") + ")"
	}
	fmt.Fprintln(a.Err, redact(clean(line)))
	if e.hint != "" {
		fmt.Fprintln(a.Err, redact(clean("hint: "+e.hint)))
	}
	ids := make([]string, len(e.jobIDs))
	for i, id := range e.jobIDs {
		ids[i] = clean(id)
	}
	switch len(ids) {
	case 0:
	case 1:
		fmt.Fprintf(a.Err, "The job %s carries on, and its credits are held. Wait for it again with:\n", ids[0])
	default:
		fmt.Fprintf(a.Err, "The jobs %s carry on, and their credits are held. Wait for them again with:\n",
			strings.Join(ids, ", "))
	}
	for _, id := range ids {
		fmt.Fprintf(a.Err, "  verifyinboxes list wait %s\n", id)
	}
	return e.exit
}
