// Package cli is the verifyinboxes command-line tool.
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/verifyinboxes/verifyinboxes-go"
	"golang.org/x/term"
)

// loginTimeout is how long a browser sign-in may take.
const loginTimeout = 5 * time.Minute

// BuildInfo is what the binary knows about itself.
type BuildInfo struct {
	Version, Commit, Date string
}

// String is the version, then the commit and the date when known: "0.1.0 (abc1234, 2026-11-18T12:00:00Z)".
func (b BuildInfo) String() string {
	out := b.Version
	var extra []string
	for _, s := range []string{b.Commit, b.Date} {
		if s != "" {
			extra = append(extra, s)
		}
	}
	if len(extra) > 0 {
		out += " (" + strings.Join(extra, ", ") + ")"
	}
	return out
}

// App is one run of the tool: where it reads and writes, and what its global flags said.
type App struct {
	In       io.Reader
	Out, Err io.Writer
	// InTTY, OutTTY and ErrTTY say whether the three are a terminal.
	InTTY, OutTTY, ErrTTY bool
	// ReadSecret reads a line from the terminal without showing it.
	ReadSecret func() (string, error)
	// OpenBrowser opens an address in the person's browser.
	OpenBrowser func(address string) error
	Build       BuildInfo
	// LoginTimeout is how long a browser sign-in may take; zero is 5 minutes.
	LoginTimeout time.Duration

	jsonOut, quiet, noColor bool
	baseURLFlag             string

	// started is set once a command's own code runs: an error before that is a mistake in the command line.
	started bool
	creds   *credStore
}

// Main runs the tool with the process's arguments and streams, and returns its exit code. The first Ctrl-C stops
// what the tool is doing (and names any job it made); a second one ends it at once.
func Main(build BuildInfo) int {
	app := &App{
		In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Build: build, OpenBrowser: openBrowser,
		InTTY: term.IsTerminal(int(os.Stdin.Fd())), OutTTY: term.IsTerminal(int(os.Stdout.Fd())),
		ErrTTY: term.IsTerminal(int(os.Stderr.Fd())),
	}
	app.ReadSecret = func() (string, error) {
		secret, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(secret), err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	return app.Run(ctx, os.Args[1:])
}

// Run runs the tool with these arguments and returns its exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	root := a.rootCommand()
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		return a.report(ctx, err)
	}
	return exitOK
}

func (a *App) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "verifyinboxes",
		Short: "Check email addresses with VerifyInboxes",
		Long: `verifyinboxes checks email addresses with the VerifyInboxes API: one at a time, a few, or whole lists.

Sign in once with your VerifyInboxes account (or save an organisation's API key):

  verifyinboxes login
  verifyinboxes verify ann@example.com
  verifyinboxes list run prospects.csv -o results.csv

VERIFYINBOXES_API_KEY, when set, is used before anything saved.

Exit codes: 0 done · 1 an error · 2 a mistake in the command line · 3 --fail-on found what it looks for ·
130 interrupted.`,
		Example: `  # A few addresses, for a script
  verifyinboxes verify ann@example.com bob@example.com --json

  # Many, one per line
  verifyinboxes verify < addresses.txt

  # A CSV, waited for, with the results added to it
  verifyinboxes list run prospects.csv --column work_email -o results.csv`,
		Version:       a.Build.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(*cobra.Command, []string) {
			a.started = true
		},
	}
	group(root)
	root.SetIn(a.In)
	root.SetOut(a.Out)
	root.SetErr(a.Err)
	root.SetVersionTemplate("verifyinboxes {{.Version}}\n")
	root.CompletionOptions.HiddenDefaultCmd = true
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return usageError(err.Error(), "See `"+cmd.CommandPath()+" --help`.")
	})

	flags := root.PersistentFlags()
	flags.BoolVar(&a.jsonOut, "json", false, "print JSON: one object per line")
	flags.StringVar(&a.baseURLFlag, "base-url", "", "the API's `address` (default "+envBaseURL+", else "+verifyinboxes.DefaultBaseURL+")")
	flags.BoolVarP(&a.quiet, "quiet", "q", false, "no progress and no notes on standard error; errors are still printed")
	flags.BoolVar(&a.noColor, "no-color", false, "no colours (they are also off when NO_COLOR is set or the output is not a terminal)")

	root.AddGroup(&cobra.Group{ID: "check", Title: "Checking addresses:"}, &cobra.Group{ID: "setup", Title: "Account and setup:"})
	root.AddCommand(a.cmdVerify(), a.cmdList(), a.cmdDomain(), a.cmdLogin(), a.cmdLogout(), a.cmdAccount(),
		a.cmdWebhooks(), a.cmdSuppressions(), a.cmdMcp())
	return root
}

// userAgent is the tool's product token, put in front of the SDK's.
func (a *App) userAgent() string {
	version := a.Build.Version
	if version == "" || !visible(version) {
		version = "dev"
	}
	return "verifyinboxes-cli/" + version
}

// client is an SDK client with the credential the API commands use: VERIFYINBOXES_API_KEY, else the key saved for
// the base URL, else the sign-in saved for the API.
func (a *App) client() (*verifyinboxes.Client, error) {
	base, err := a.baseURL()
	if err != nil {
		return nil, err
	}
	opts := []verifyinboxes.Option{verifyinboxes.WithBaseURL(base), verifyinboxes.WithUserAgent(a.userAgent())}
	if key := envKey(); key != "" {
		opts = append(opts, verifyinboxes.WithAPIKey(key))
	} else {
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
			opts = append(opts, verifyinboxes.WithAPIKey(saved.APIKey))
		case saved.API != nil:
			opts = append(opts, verifyinboxes.WithTokenSource(a.signIn(base, forAPI, *saved.API)))
		default:
			return nil, &cliError{msg: "not signed in", exit: exitError,
				hint: "Run `verifyinboxes login` (or `verifyinboxes login --api-key`), or set " + envAPIKey + "."}
		}
	}
	return a.newClient(opts...)
}

func (a *App) newClient(opts ...verifyinboxes.Option) (*verifyinboxes.Client, error) {
	c, err := verifyinboxes.NewClient(opts...)
	if err != nil {
		return nil, &cliError{msg: strings.TrimPrefix(err.Error(), "verifyinboxes: "), exit: exitError}
	}
	return c, nil
}

// note prints a line for the person at the terminal on standard error, unless --quiet. The line is cleaned whole:
// what it names (a list's id, an address) may be the API's text.
func (a *App) note(format string, args ...any) {
	if !a.quiet {
		fmt.Fprintln(a.Err, clean(fmt.Sprintf(format, args...)))
	}
}

// readLines reads standard input's non-empty lines, trimmed.
func (a *App) readLines() ([]string, error) {
	var out []string
	scanner := bufio.NewScanner(a.In)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			out = append(out, line)
		}
	}
	return out, scanner.Err()
}

// exactArgs is cobra.ExactArgs with a message that says what is wanted, and never repeats what was given (it may
// be a key typed where it does not belong).
func exactArgs(names ...string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == len(names) {
			return nil
		}
		msg := fmt.Sprintf("`%s` takes no arguments", cmd.CommandPath())
		if len(names) > 0 {
			msg = fmt.Sprintf("`%s` takes <%s>", cmd.CommandPath(), strings.Join(names, "> <"))
		}
		return usageError(msg, "See `"+cmd.CommandPath()+" --help`.")
	}
}

// group makes cmd a command of subcommands: alone it prints its help; with anything else, it is a mistake.
func group(cmd *cobra.Command) {
	cmd.Args = cobra.ArbitraryArgs
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if len(args) > 0 {
			return usageError(fmt.Sprintf("unknown command %q for `%s`", args[0], c.CommandPath()),
				"See `"+c.CommandPath()+" --help`.")
		}
		return c.Help()
	}
}

// atLeastArgs wants one argument or more.
func atLeastArgs(name string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return nil
		}
		return usageError(fmt.Sprintf("`%s` takes <%s>...", cmd.CommandPath(), name), "See `"+cmd.CommandPath()+" --help`.")
	}
}

// visible reports whether s is one or more of the characters ! to ~.
func visible(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '!' || s[i] > '~' {
			return false
		}
	}
	return true
}
