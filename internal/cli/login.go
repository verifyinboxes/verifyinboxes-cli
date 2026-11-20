package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/verifyinboxes/verifyinboxes-go"
)

func (a *App) cmdLogin() *cobra.Command {
	var forWhat string
	var noBrowser, apiKey bool
	cmd := &cobra.Command{
		Use:     "login",
		GroupID: "setup",
		Short:   "Sign in with your VerifyInboxes account, or save an API key",
		Long: `Sign in with your VerifyInboxes account in your browser. The commands then act as you, in the organisation
you choose, with your role there. The sign-in is kept in your system's keychain (or, where there is none, in a file
only you can read) and renewed by itself; it lasts until you sign out, or 30 days without use.

--for mcp signs in for AI apps instead: "verifyinboxes mcp" uses it. It is a sign-in of its own, listed in the app's
Connected apps as "For MCP".

--no-browser is for a computer without a browser (a server over SSH): it prints the address to open elsewhere,
then takes the address the browser ends on, pasted.

--api-key saves an organisation's API key instead (an organisation admin makes keys in the app, under Developers
-> API keys). The key is read from standard input, never from the command line: on a terminal it is asked for
without being shown. VERIFYINBOXES_API_KEY, when set, is used before anything saved.`,
		Example: `  verifyinboxes login
  verifyinboxes login --for mcp
  verifyinboxes login --no-browser
  verifyinboxes login --api-key
  printf '%s\n' "$KEY" | verifyinboxes login --api-key`,
		Args: exactArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			kind := resourceKind(forWhat)
			if kind != forAPI && kind != forMCP {
				return usageError("--for takes api or mcp", "See `verifyinboxes login --help`.")
			}
			if apiKey {
				if cmd.Flags().Changed("for") || noBrowser {
					return usageError("--api-key takes no --for and no --no-browser: a key works for both",
						"See `verifyinboxes login --help`.")
				}
				return a.loginWithKey(cmd.Context())
			}
			return a.login(cmd.Context(), kind, noBrowser)
		},
	}
	cmd.Flags().StringVar(&forWhat, "for", "api", "what to sign in for: `api` (the commands) or mcp (AI apps, through verifyinboxes mcp)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the address instead of opening a browser, and take the address it ends on, pasted")
	cmd.Flags().BoolVar(&apiKey, "api-key", false, "save an API key read from standard input instead of signing in")
	return cmd
}

// login signs in with the browser and saves the sign-in; an older one for the same purpose is revoked.
func (a *App) login(ctx context.Context, kind resourceKind, noBrowser bool) error {
	base, err := a.baseURL()
	if err != nil {
		return err
	}
	store, err := a.store()
	if err != nil {
		return err
	}
	cred, auth, err := a.browserLogin(ctx, base, kind, noBrowser)
	if err != nil {
		return err
	}
	var previous *OAuthCredential
	droppedKey := false
	if err := store.update(base, func(e *savedEntry) {
		previous = *kind.of(e)
		*kind.of(e) = cred
		if kind == forAPI && e.APIKey != "" {
			// The person chose to act as themselves: a key saved before would be used first.
			e.APIKey, droppedKey = "", true
		}
	}); err != nil {
		return err
	}
	if previous != nil && previous.RefreshToken != "" {
		// The sign-in this one replaces is ended, so that Connected apps does not keep it.
		revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_ = auth.revoke(revokeCtx, previous.ClientID, previous.RefreshToken)
		cancel()
	}
	if droppedKey {
		a.note("The API key saved before was removed: the commands now act as you.")
	}
	if kind == forMCP {
		fmt.Fprintln(a.Out, "Signed in for AI apps: verifyinboxes mcp now works.")
		if saved, err := store.load(base); err == nil && saved.APIKey != "" {
			a.note("An API key is saved too, and verifyinboxes mcp uses it first: `verifyinboxes logout` removes both.")
		}
		return nil
	}
	client, err := a.newClient(verifyinboxes.WithBaseURL(base), verifyinboxes.WithUserAgent(a.userAgent()),
		verifyinboxes.WithTokenSource(a.signIn(base, forAPI, *cred)))
	if err != nil {
		return err
	}
	account, err := client.Account.Get(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "Signed in to %s.\n", clean(account.Name))
	a.warnEnvKey()
	return nil
}

func (a *App) warnEnvKey() {
	if envKey() != "" {
		a.note("%s is set in this shell, and is used before anything saved.", envAPIKey)
	}
}

// loginWithKey reads a key from standard input, checks it with GET /v1/account and saves it.
func (a *App) loginWithKey(ctx context.Context) error {
	base, err := a.baseURL()
	if err != nil {
		return err
	}
	key, err := a.askKey()
	if err != nil {
		return err
	}
	client, err := a.newClient(verifyinboxes.WithBaseURL(base), verifyinboxes.WithUserAgent(a.userAgent()),
		verifyinboxes.WithAPIKey(key))
	if err != nil {
		return err
	}
	account, err := client.Account.Get(ctx)
	if err != nil {
		var api *verifyinboxes.APIError
		if errors.As(err, &api) && errors.Is(err, verifyinboxes.ErrAuthentication) {
			return &cliError{msg: "the API did not take that key; nothing was saved", code: api.Code, status: api.StatusCode,
				requestID: api.RequestID, exit: exitError,
				hint: "Check that you copied all of it. An organisation admin makes keys in the app, under Developers -> API keys."}
		}
		return err
	}
	store, err := a.store()
	if err != nil {
		return err
	}
	hasSignIn := false
	if err := store.update(base, func(e *savedEntry) {
		e.APIKey = key
		hasSignIn = e.API != nil || e.MCP != nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "Saved the API key for %s.\n", clean(account.Name))
	if hasSignIn {
		a.note("The key is used before your sign-in: the commands now act as the organisation.")
	}
	a.warnEnvKey()
	return nil
}

// askKey reads the key: on a terminal without showing it, else the first line of standard input.
func (a *App) askKey() (string, error) {
	var key string
	if a.InTTY && a.ReadSecret != nil {
		fmt.Fprint(a.Err, "API key (it is not shown): ")
		secret, err := a.ReadSecret()
		if err != nil {
			return "", &cliError{msg: "could not read the key: " + err.Error(), exit: exitError}
		}
		key = strings.TrimSpace(secret)
	} else {
		lines, err := a.readLines()
		if err != nil && !errors.Is(err, io.EOF) {
			return "", &cliError{msg: "could not read the key: " + err.Error(), exit: exitError}
		}
		if len(lines) > 0 {
			key = lines[0]
		}
	}
	if key == "" {
		return "", usageError("no key was given",
			"Paste it when asked, or pipe it in: printf '%s\\n' \"$KEY\" | verifyinboxes login --api-key")
	}
	return key, nil
}

func (a *App) cmdLogout() *cobra.Command {
	return &cobra.Command{
		Use:     "logout",
		GroupID: "setup",
		Short:   "Sign out, and forget what is saved",
		Long: `End the sign-ins saved for the API's address (the API is told, so they stop working at once) and delete
them, and any API key saved, from this computer. A key itself keeps working: an organisation admin revokes it in
the app, under Developers -> API keys.`,
		Example: `  verifyinboxes logout`,
		Args:    exactArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			base, err := a.baseURL()
			if err != nil {
				return err
			}
			store, err := a.store()
			if err != nil {
				return err
			}
			saved, err := store.load(base)
			if err != nil {
				return err
			}
			if saved.API != nil || saved.MCP != nil {
				ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
				defer cancel()
				auth, err := a.discover(ctx, base)
				for _, cred := range []*OAuthCredential{saved.API, saved.MCP} {
					if cred == nil || err != nil {
						continue
					}
					if revokeErr := auth.revoke(ctx, cred.ClientID, cred.RefreshToken); revokeErr != nil {
						err = revokeErr
					}
				}
				if err != nil {
					a.note("The API could not be told (%s): the sign-in ends by itself after 30 days unused, or end it now "+
						"in the app, under Connected apps.", redact(err.Error()))
				}
			}
			if err := store.remove(base); err != nil {
				return err
			}
			fmt.Fprintln(a.Out, "Signed out.")
			if envKey() != "" {
				a.note("%s is still set in this shell.", envAPIKey)
			}
			return nil
		},
	}
}
