package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/verifyinboxes/verifyinboxes-go"
)

// rowFilter is the --only and --verdicts flags: which rows of a list's results.
type rowFilter struct {
	only     string
	verdicts string
	partial  bool
}

func (f *rowFilter) add(cmd *cobra.Command, partial bool) {
	cmd.Flags().StringVar(&f.only, "only", "", "only the rows that are `safe-to-send`")
	cmd.Flags().StringVar(&f.verdicts, "verdicts", "", "only the rows of these `verdicts`, comma-separated: valid, invalid, catch_all, risky, unknown")
	if partial {
		cmd.Flags().BoolVar(&f.partial, "partial", false, "a list still running, as it stands")
	}
}

func (f *rowFilter) check() error {
	if f.only != "" && f.only != "safe-to-send" {
		return usageError("--only takes safe-to-send", "Use --verdicts for verdicts.")
	}
	if f.only != "" && f.verdicts != "" {
		return usageError("give --only or --verdicts, not both", "")
	}
	return nil
}

func (f *rowFilter) set() bool { return f.only != "" || f.verdicts != "" }

func (f *rowFilter) verdictList() []string {
	if f.verdicts == "" {
		return nil
	}
	var out []string
	for _, v := range strings.Split(f.verdicts, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (f *rowFilter) download() *verifyinboxes.DownloadParams {
	return &verifyinboxes.DownloadParams{OnlySafeToSend: f.only != "", Verdicts: f.verdictList(), Partial: f.partial}
}

func (f *rowFilter) results() *verifyinboxes.ResultsParams {
	return &verifyinboxes.ResultsParams{PageSize: 1000, OnlySafeToSend: f.only != "", Verdicts: f.verdictList(), Partial: f.partial}
}

func (a *App) cmdList() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		GroupID: "check",
		Short:   "Run a CSV of addresses as a list, and read its results",
		Long: `A list is a CSV of addresses checked together: start one with "list run", then wait for it, read its results
or download the CSV with the results appended. A few hundred addresses take minutes, Microsoft-hosted ones longer.
Each address costs 1 credit when its verdict is valid, invalid, catch_all or risky; unknown is free.

"list wait" also picks up a check that "verify" left running.`,
	}
	group(cmd)
	cmd.AddCommand(a.cmdListRun(), a.cmdListStatus(), a.cmdListWait(), a.cmdListResults(), a.cmdListDownload(), a.cmdListResolve())
	return cmd
}

func (a *App) cmdListRun() *cobra.Command {
	var column, output, failOn string
	var wait bool
	var timeout time.Duration
	var filter rowFilter
	cmd := &cobra.Command{
		Use:   "run <file.csv>",
		Short: "Start a list from a CSV",
		Long: `Start a list from a CSV with a header row; --column names the column of addresses (default email).

Without --wait it prints the list's id and returns at once. --wait waits for it (--timeout, default as long as it
takes), showing its progress on standard error; -o waits too, then downloads the CSV with the results appended to
that file (--only or --verdicts for some rows only). If the wait runs out or you press Ctrl-C, the list carries
on: "verifyinboxes list wait <id>" picks it up again.

--fail-on invalid exits 3 when some address is invalid; --fail-on unsafe when some is not safe to send to.`,
		Example: `  verifyinboxes list run prospects.csv
  verifyinboxes list run prospects.csv --column work_email -o results.csv
  verifyinboxes list run prospects.csv -o safe.csv --only safe-to-send`,
		Args: exactArgs("file.csv"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := filter.check(); err != nil {
				return err
			}
			if err := checkFailOn(failOn); err != nil {
				return err
			}
			if filter.set() && output == "" {
				return usageError("--only and --verdicts choose the rows written to -o: give -o too", "")
			}
			ctx := cmd.Context()
			client, err := a.client()
			if err != nil {
				return err
			}
			created, err := a.startList(ctx, func(ctx context.Context) (*verifyinboxes.JobCreated, error) {
				return client.Lists.CreateFromFile(ctx, args[0], &verifyinboxes.CreateListParams{EmailColumn: column})
			})
			if err != nil {
				return err
			}
			if !wait && output == "" && failOn == "" {
				if a.jsonOut {
					return a.printJSON(created)
				}
				fmt.Fprintf(a.Out, "Started list %s with %d addresses.\n", clean(created.ID), created.Total)
				a.note("Wait for it: verifyinboxes list wait %s", created.ID)
				return nil
			}
			a.note("Started list %s with %d addresses.", created.ID, created.Total)
			job, err := a.waitList(ctx, client, created.ID, timeout)
			if err != nil {
				return err
			}
			if output != "" {
				if _, err := client.Lists.DownloadFile(ctx, job.ID, output, filter.download()); err != nil {
					return withJobs(err, job.ID)
				}
			}
			if err := a.printJob(job); err != nil {
				return err
			}
			if output != "" && !a.jsonOut {
				fmt.Fprintf(a.Out, "Saved the results to %s.\n", output)
			}
			return a.failOnList(ctx, client, job, failOn)
		},
	}
	cmd.Flags().StringVar(&column, "column", "email", "the CSV's `header` of the column of addresses")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for the list to finish")
	cmd.Flags().StringVarP(&output, "output", "o", "", "wait, then write the CSV with the results to this `file`")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "how long to wait (0: as long as it takes)")
	cmd.Flags().StringVar(&failOn, "fail-on", "", "wait, then exit 3 when some address is `invalid`, or (unsafe) not safe to send to")
	filter.add(cmd, false)
	return cmd
}

// failOnList is --fail-on for a finished list.
func (a *App) failOnList(ctx context.Context, client *verifyinboxes.Client, job *verifyinboxes.Job, failOn string) error {
	switch failOn {
	case "invalid":
		n := 0
		if job.Counts != nil {
			n = job.Counts.Invalid
		}
		return failOnCount(failOn, n, job.Total)
	case "unsafe":
		safe := 0
		it := client.Lists.Results(ctx, job.ID, &verifyinboxes.ResultsParams{PageSize: 1000, OnlySafeToSend: true})
		for it.Next() {
			safe++
		}
		if err := it.Err(); err != nil {
			return withJobs(err, job.ID)
		}
		return failOnCount(failOn, job.Total-safe, job.Total)
	}
	return nil
}

func (a *App) cmdListStatus() *cobra.Command {
	return &cobra.Command{
		Use:     "status <id>",
		Short:   "Show a list (or a check) as it is now",
		Example: `  verifyinboxes list status 019a...`,
		Args:    exactArgs("id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			job, err := client.Lists.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.printJob(job)
		},
	}
}

func (a *App) cmdListWait() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait <id>",
		Short: "Wait for a list (or a check) to finish",
		Long: `Wait for a list, a check or a resolution to finish, showing its progress on standard error, then show it.
This is how a list or a check is picked up again after a wait ran out or was interrupted.`,
		Example: `  verifyinboxes list wait 019a...
  verifyinboxes list wait 019a... --timeout 30m`,
		Args: exactArgs("id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			job, err := a.waitList(cmd.Context(), client, args[0], timeout)
			if err != nil {
				return err
			}
			return a.printJob(job)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "how long to wait (0: as long as it takes)")
	return cmd
}

func (a *App) cmdListResults() *cobra.Command {
	var filter rowFilter
	cmd := &cobra.Command{
		Use:   "results <id>",
		Short: "Print a finished list's results, one JSON object per row",
		Long: `Print a finished list's results in row order, one JSON object per row (with or without --json): the row's
number in the CSV, the address and its result. For a CSV, use "list download".`,
		Example: `  verifyinboxes list results 019a...
  verifyinboxes list results 019a... --only safe-to-send`,
		Args: exactArgs("id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := filter.check(); err != nil {
				return err
			}
			client, err := a.client()
			if err != nil {
				return err
			}
			it := client.Lists.Results(cmd.Context(), args[0], filter.results())
			for it.Next() {
				if err := a.printJSON(it.Current()); err != nil {
					return err
				}
			}
			return it.Err()
		},
	}
	filter.add(cmd, true)
	return cmd
}

func (a *App) cmdListDownload() *cobra.Command {
	var output string
	var filter rowFilter
	cmd := &cobra.Command{
		Use:   "download <id>",
		Short: "Download the CSV with the results appended",
		Long: `Download the CSV you sent with the results appended: to standard output, or to a file with -o (written whole
or not at all).`,
		Example: `  verifyinboxes list download 019a... -o results.csv
  verifyinboxes list download 019a... --verdicts valid,catch_all > keep.csv`,
		Args: exactArgs("id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := filter.check(); err != nil {
				return err
			}
			client, err := a.client()
			if err != nil {
				return err
			}
			if output == "" {
				_, err := client.Lists.Download(cmd.Context(), args[0], a.Out, filter.download())
				return err
			}
			n, err := client.Lists.DownloadFile(cmd.Context(), args[0], output, filter.download())
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(map[string]any{"file": output, "bytes": n})
			}
			fmt.Fprintf(a.Out, "Saved the results to %s.\n", output)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "write the CSV to this `file` instead of standard output")
	filter.add(cmd, true)
	return cmd
}

func (a *App) cmdListResolve() *cobra.Command {
	var emails []string
	cmd := &cobra.Command{
		Use:   "resolve <id>",
		Short: "Settle a finished job's catch_all and risky addresses with test emails",
		Long: `Send a finished list's (or check's) catch_all and risky addresses, all of them or those given with --email,
to be settled by test emails. The resolution is a job of its own: wait for it with "verifyinboxes list wait".
It needs test emails turned on for the organisation.`,
		Example: `  verifyinboxes list resolve 019a...
  verifyinboxes list resolve 019a... --email ann@example.com`,
		Args: exactArgs("id"),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			created, err := client.Lists.Resolve(cmd.Context(), args[0], &verifyinboxes.ResolveParams{Emails: emails})
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(created)
			}
			fmt.Fprintf(a.Out, "Resolving %d addresses as job %s (%s).\n", created.Total, clean(created.ID),
				clean(string(created.Status)))
			s := created.Skipped
			if s.Ineligible+s.Excluded+s.AlreadyRequested+s.Duplicate > 0 {
				fmt.Fprintf(a.Out, "Left out: %d not catch_all or risky, %d excluded, %d already sent, %d repeated.\n",
					s.Ineligible, s.Excluded, s.AlreadyRequested, s.Duplicate)
			}
			if created.Status != verifyinboxes.JobDone {
				a.note("Wait for it: verifyinboxes list wait %s", created.ID)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&emails, "email", nil, "only this `address` (repeat it for more)")
	return cmd
}
