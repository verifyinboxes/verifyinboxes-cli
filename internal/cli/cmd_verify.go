package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/verifyinboxes/verifyinboxes-go"
)

// singlesAtMost is how many addresses are checked one by one; more make one list (as the SDK's VerifyMany does).
const singlesAtMost = 10

func (a *App) cmdVerify() *cobra.Command {
	var timeout time.Duration
	var failOn string
	cmd := &cobra.Command{
		Use:     "verify <email>...",
		GroupID: "check",
		Short:   "Check addresses, given or one per line on standard input",
		Long: `Check email addresses: those given, or, with none given, one per line on standard input.

Up to 10 are checked one by one, together; more go as one list, whose progress is shown on standard error.
A few hundred addresses take minutes, Microsoft-hosted ones longer.
Each address costs 1 credit when its verdict is valid, invalid, catch_all or risky; unknown is free.

If the wait runs out (--timeout, default 10 minutes; 0 waits as long as it takes) or you press Ctrl-C, the checks
carry on: the error names their job ids, and "verifyinboxes list wait <id>" picks them up again.

--fail-on invalid exits 3 when some address is invalid; --fail-on unsafe when some is not safe to send to.`,
		Example: `  verifyinboxes verify ann@example.com
  verifyinboxes verify ann@example.com bob@example.com --json
  verifyinboxes verify --fail-on invalid < addresses.txt`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkFailOn(failOn); err != nil {
				return err
			}
			emails := args
			if len(emails) == 0 {
				if a.InTTY {
					return usageError("give one or more addresses, or pipe them in one per line",
						"verifyinboxes verify ann@example.com, or verifyinboxes verify < addresses.txt")
				}
				lines, err := a.readLines()
				if err != nil {
					return &cliError{msg: "could not read standard input: " + err.Error(), exit: exitError}
				}
				emails = lines
			}
			if len(emails) == 0 {
				return usageError("no addresses were given", "Give them as arguments, or one per line on standard input.")
			}
			client, err := a.client()
			if err != nil {
				return err
			}
			var results []verifyinboxes.Result
			if len(emails) <= singlesAtMost {
				wait := timeout
				if wait <= 0 {
					wait = -1 // as long as it takes
				}
				results, err = client.VerifyMany(cmd.Context(), emails, &verifyinboxes.VerifyManyParams{Timeout: wait})
			} else {
				results, err = a.verifyAsList(cmd.Context(), client, emails, timeout)
			}
			if err != nil {
				return err
			}
			if err := a.printResults(results); err != nil {
				return err
			}
			return failOnResults(failOn, results)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "how long to wait for the results (0: as long as it takes)")
	cmd.Flags().StringVar(&failOn, "fail-on", "", "exit 3 when some address is `invalid`, or (unsafe) not safe to send to")
	return cmd
}

// verifyAsList checks many addresses as one list: made, waited for with progress, and read back in order.
func (a *App) verifyAsList(ctx context.Context, client *verifyinboxes.Client, emails []string, timeout time.Duration) ([]verifyinboxes.Result, error) {
	created, err := a.startList(ctx, func(ctx context.Context) (*verifyinboxes.JobCreated, error) {
		return client.Lists.Create(ctx, &verifyinboxes.CreateListParams{Emails: emails})
	})
	if err != nil {
		return nil, err
	}
	a.note("Checking %d addresses as list %s.", created.Total, created.ID)
	job, err := a.waitList(ctx, client, created.ID, timeout)
	if err != nil {
		return nil, err
	}
	rows := map[int]verifyinboxes.ResultRow{}
	it := client.Lists.Results(ctx, created.ID, &verifyinboxes.ResultsParams{PageSize: 1000})
	for it.Next() {
		row := it.Current()
		rows[row.Row] = row
	}
	if err := it.Err(); err != nil {
		return nil, withJobs(err, job.ID)
	}
	results := make([]verifyinboxes.Result, len(emails))
	for i, email := range emails {
		row, ok := rows[i+1]
		if !ok {
			return nil, withJobs(fmt.Errorf("the list %s has no result for row %d (%s)", job.ID, i+1, email), job.ID)
		}
		results[i] = row.Result
		results[i].JobID = job.ID
	}
	return results, nil
}

// startList makes a list. Once its request is sent it is let finish, Ctrl-C or not, so that the list's id is known
// and named: an interrupted upload would otherwise leave a list nobody knows of, its credits held.
func (a *App) startList(ctx context.Context, create func(context.Context) (*verifyinboxes.JobCreated, error)) (*verifyinboxes.JobCreated, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sent, noted := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(noted)
		select {
		case <-ctx.Done():
			a.note("Interrupted: waiting for the list's answer, so that its id is not lost. Press Ctrl-C again to quit now.")
		case <-sent:
		}
	}()
	created, err := create(context.WithoutCancel(ctx))
	close(sent)
	<-noted
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, withJobs(err, created.ID)
	}
	return created, nil
}

// listPauses are the pauses between two looks at a running list: 2, 4, 8, 16, then every 30 seconds.
var listPauses = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}

// waitList waits for a job to finish (timeout 0: as long as it takes), showing its progress on standard error.
// Every error names the job.
func (a *App) waitList(ctx context.Context, client *verifyinboxes.Client, id string, timeout time.Duration) (*verifyinboxes.Job, error) {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	job, err := client.Lists.Get(ctx, id)
	if err != nil {
		return nil, withJobs(err, id)
	}
	shown := -1
	progress := func(final bool) {
		if a.quiet || (job.Done == shown && !final) {
			return
		}
		shown = job.Done
		// The escape sequence that rewrites the line is the tool's own; the job's id is cleaned.
		switch {
		case a.ErrTTY && final:
			fmt.Fprintf(a.Err, "\r\x1b[K%d of %d checked.\n", job.Done, job.Total)
		case a.ErrTTY:
			fmt.Fprintf(a.Err, "\r\x1b[K%d of %d checked...", job.Done, job.Total)
		default:
			fmt.Fprintf(a.Err, "%s: %d of %d checked.\n", clean(job.ID), job.Done, job.Total)
		}
	}
	for asked := 0; job.Status == verifyinboxes.JobRunning || job.Status == verifyinboxes.JobQueued; asked++ {
		progress(false)
		pause := listPauses[min(asked, len(listPauses)-1)]
		if !deadline.IsZero() {
			left := time.Until(deadline)
			if left <= 0 {
				if a.ErrTTY && !a.quiet {
					fmt.Fprintln(a.Err)
				}
				return nil, &waitTimeout{jobID: id, waited: timeout}
			}
			pause = min(pause, left)
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			if a.ErrTTY && !a.quiet {
				fmt.Fprintln(a.Err)
			}
			return nil, withJobs(ctx.Err(), id)
		case <-timer.C:
		}
		if job, err = client.Lists.Get(ctx, id); err != nil {
			return nil, withJobs(err, id)
		}
	}
	if job.Kind != "single" {
		progress(true)
	}
	if job.Status != verifyinboxes.JobDone {
		return nil, withJobs(fmt.Errorf("the job %s ended %s", id, job.Status), id)
	}
	return job, nil
}

func checkFailOn(failOn string) error {
	if failOn != "" && failOn != "invalid" && failOn != "unsafe" {
		return usageError("--fail-on takes invalid or unsafe", "See `verifyinboxes verify --help`.")
	}
	return nil
}

// failOnResults is exit 3 when --fail-on finds what it looks for among the results.
func failOnResults(failOn string, results []verifyinboxes.Result) error {
	n := 0
	for _, r := range results {
		if (failOn == "invalid" && r.Verdict == verifyinboxes.VerdictInvalid) || (failOn == "unsafe" && !r.SafeToSend) {
			n++
		}
	}
	return failOnCount(failOn, n, len(results))
}

func failOnCount(failOn string, n, of int) error {
	if failOn == "" || n == 0 {
		return nil
	}
	what := "invalid"
	if failOn == "unsafe" {
		what = "not safe to send to"
	}
	return &cliError{msg: fmt.Sprintf("--fail-on %s: %d of %d addresses %s", failOn, n, of, what), exit: exitFailOn, plain: true}
}
