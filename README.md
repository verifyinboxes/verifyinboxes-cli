# VerifyInboxes command-line tool

`verifyinboxes` checks email addresses with [VerifyInboxes](https://verifyinboxes.com) from a terminal or a script:
one address, a few, or a whole CSV. It also gives AI apps the VerifyInboxes MCP server over stdio.

```console
$ verifyinboxes login
Signed in to Acme.
$ verifyinboxes verify ann@example.com bob@example.com
EMAIL            VERDICT  SAFE TO SEND  REASON
ann@example.com  valid    yes           mailbox_exists
bob@example.com  invalid  no            mailbox_not_found
$ verifyinboxes list run prospects.csv -o results.csv
```

## Install

On Linux or macOS:

```sh
curl -fsSL https://verifyinboxes.com/install.sh | sh
```

It downloads the release for your system, checks it against the release's `checksums.txt`, and puts `verifyinboxes`
in `/usr/local/bin` (when you can write there) or `~/.local/bin`. It reads:

| Variable | |
| --- | --- |
| `VERIFYINBOXES_VERSION` | the version to install, e.g. `0.1.0` (default: the latest release) |
| `VERIFYINBOXES_INSTALL_DIR` | where to put the binary |
| `VERIFYINBOXES_REPO` | the GitHub repository of the releases (default `verifyinboxes/verifyinboxes-cli`) |
| `VERIFYINBOXES_DOWNLOAD_BASE` | where the release files are, instead of GitHub (needs `VERIFYINBOXES_VERSION`) |

With [Homebrew](https://brew.sh), on macOS or Linux:

```sh
brew install verifyinboxes/tap/verifyinboxes
```

and `brew upgrade verifyinboxes` for a newer release.

Or download it from the [releases](https://github.com/verifyinboxes/verifyinboxes-cli/releases) page. Each release
lists the archives for Linux, macOS and Windows on amd64 and arm64, and their SHA-256 checksums in `checksums.txt`.
On Windows, download `verifyinboxes_<version>_windows_amd64.zip` (or `_arm64`) and put `verifyinboxes.exe` on your
`PATH`.

With Go 1.26 or later you can also run `go install github.com/verifyinboxes/verifyinboxes-cli/cmd/verifyinboxes@latest`.

`verifyinboxes --version` prints the version you have.

## Sign in

```sh
verifyinboxes login
```

opens the VerifyInboxes sign-in page in your browser. Sign in, choose the organisation, and allow access: the
commands then act as you, in that organisation, with your role there (a member sees only their own lists). The
browser comes back to the tool on `127.0.0.1` and shows "Signed in. You can close this tab."; the terminal says
`Signed in to <organisation>.` The sign-in is listed in the app under **Connected apps**, where you can end it.

| | |
| --- | --- |
| `verifyinboxes login` | Signs in for the commands. Signing in again ends the sign-in it replaces. |
| `verifyinboxes login --for mcp` | Signs in for AI apps, for `verifyinboxes mcp` (a sign-in of its own, listed as "For MCP"). |
| `verifyinboxes login --no-browser` | For a computer without a browser, such as a server over SSH: it prints the address to open on another computer, and takes the address that browser ends on, pasted into the terminal. |
| `verifyinboxes login --api-key` | Saves an organisation's API key instead (an organisation admin makes keys in the app, under **Developers → API keys**). |
| `verifyinboxes logout` | Ends the saved sign-ins (the API is told, so they stop working at once) and deletes them and any saved key from this computer. |

A key is never taken on the command line, where your shell's history and other users' process lists would see it.
`login --api-key` reads it from standard input: on a terminal it asks for it without showing it; in a script, pipe
it in:

```sh
printf '%s\n' "$VERIFYINBOXES_KEY" | verifyinboxes login --api-key
```

Or skip saving anything: when `VERIFYINBOXES_API_KEY` is set, every command uses it, before anything saved.

**Which credential a command uses.** `VERIFYINBOXES_API_KEY`, else the key saved for the API's address, else your
sign-in (`login`; `login --for mcp` for `verifyinboxes mcp`).

**Where it is kept.** In your system's keychain: the macOS Keychain, the Windows Credential Manager, or the Secret
Service on Linux (GNOME Keyring, KWallet), under the name `verifyinboxes-cli`, one entry per API address. Where there
is no keychain (a server, a container), in a file only you can read, and the tool says so the first time:

| System | File |
| --- | --- |
| Linux | `~/.config/verifyinboxes/credentials.json` (or `$XDG_CONFIG_HOME/verifyinboxes/credentials.json`) |
| macOS | `~/Library/Application Support/verifyinboxes/credentials.json` |
| Windows | `%AppData%\verifyinboxes\credentials.json` |

Set `VERIFYINBOXES_CONFIG_DIR` to keep it in `credentials.json` in another directory instead of the keychain. The
file is readable by you alone, and written whole or not at all. A directory the tool makes is readable by you alone
too; one that exists already keeps its permissions, so give it yours alone (`chmod 700`).

A keychain that stops answering in the middle of a command (locked meanwhile) makes the command fail, rather than
save what it renewed somewhere the keychain's older entry would hide: unlock it and run the command again.

**How long it lasts.** A sign-in's access token lives an hour; the tool renews it by itself (a minute before it
ends, or when the API refuses it) and saves the new one before using it, so several commands running at once share
one sign-in. It lasts until you sign out, end it in **Connected apps**, change your password, or leave it unused for
30 days. When it has ended, the tool says `sign in again: verifyinboxes login`.

## Checking addresses

```sh
verifyinboxes verify ann@example.com
verifyinboxes verify ann@example.com bob@example.com --json
verifyinboxes verify < addresses.txt          # one address per line
```

Up to 10 addresses are checked one by one, together. More go as one list, whose progress is shown on standard error.
Don't loop over `verify` one address at a time for many addresses: give them all at once, or use a list. A few
hundred addresses take minutes, Microsoft-hosted ones longer.

Each address costs 1 credit when its verdict is `valid`, `invalid`, `catch_all` or `risky`; `unknown` is free.

| Column (field) | |
| --- | --- |
| `EMAIL` (`email`) | The address, as checked. |
| `VERDICT` (`verdict`) | `valid`, `invalid`, `catch_all`, `risky` or `unknown`. More may be added: act on `safe_to_send`. |
| `SAFE TO SEND` (`safe_to_send`) | Whether to send to it: `valid`, or a `catch_all` whose score reaches your organisation's threshold. |
| `REASON` (`reason`) | Why: `mailbox_exists`, `mailbox_not_found`, `no_mail_server`, `accept_all`, `syntax`, `typo` (with `did_you_mean`), ... |

`--json` adds the rest: `score`, `flags`, `provider`, `catch_all_domain`, `verified_at`, `source` and `job_id`.

`verify` waits up to 10 minutes (`--timeout 30m` for longer, `--timeout 0` for as long as it takes). If the time runs
out, or you press Ctrl-C, the checks carry on and their credits are held: the error names their job ids, and
`verifyinboxes list wait <id>` picks them up again. Don't start them again.

`--fail-on invalid` exits with 3 when some address is `invalid`; `--fail-on unsafe` when some is not safe to send to.

## Lists

A list is a CSV with a header row, checked as a whole.

```sh
verifyinboxes list run prospects.csv                                # start it; prints its id
verifyinboxes list run prospects.csv --column work_email -o results.csv
verifyinboxes list run prospects.csv -o safe.csv --only safe-to-send
verifyinboxes list wait 019a...
```

| Command | |
| --- | --- |
| `verifyinboxes list run <file.csv>` | Starts a list. `--column` names the column of addresses (default `email`). `--wait` waits for it, with its progress on standard error; `-o <file>` waits, then writes the CSV with the results appended (`--only safe-to-send` or `--verdicts valid,catch_all` for some rows only); `--fail-on` waits, then exits 3 as for `verify`. `--timeout` limits the wait (default none). |
| `verifyinboxes list status <id>` | The list (or a check) as it is now: its status, how many are checked, its verdict counts. |
| `verifyinboxes list wait <id>` | Waits for it to finish (`--timeout`, default none). |
| `verifyinboxes list results <id>` | Its results in row order, one JSON object per row (`row`, `email` and the result), with or without `--json`; `--only`, `--verdicts`, `--partial` (a list still running, as it stands). |
| `verifyinboxes list download <id>` | The CSV you sent with the results appended: to standard output, or to a file with `-o` (written whole or not at all); `--only`, `--verdicts`, `--partial`. |
| `verifyinboxes list resolve <id>` | Sends a finished job's `catch_all` and `risky` addresses (all, or those given with `--email`) to be settled by test emails, as a job of its own. |

When `list run` is waiting and the wait runs out or you press Ctrl-C, the list carries on: `verifyinboxes list wait
<id>` picks it up again. Once a list's upload has been sent, Ctrl-C waits for its answer, so that its id is never
lost; press Ctrl-C again to quit at once.

## More commands

| Command | |
| --- | --- |
| `verifyinboxes account` | The organisation's name, balance and settings. |
| `verifyinboxes domain <domain>` | A domain's mail provider, MX hosts, whether it accepts mail or every address, and the usual shape of its addresses (1 credit when DNS answers). |
| `verifyinboxes webhooks list` | The webhooks. |
| `verifyinboxes webhooks add <url> [--event job.completed] [--event address.contradicted]` | Makes a webhook (default: both events). Its signing secret is shown once: store it now. |
| `verifyinboxes webhooks delete <id>` | Deletes one. |
| `verifyinboxes suppressions list` | The domains on the suppression list, and how many addresses (kept only as a hash). |
| `verifyinboxes suppressions add --email <address> --domain <domain>` | Adds addresses and domains (each flag may be repeated). A check of one is answered `invalid` (`suppressed`) and costs nothing. |

Every command has `--help`.

## Output for scripts

`--json` prints JSON on standard output, one object per line: a command that prints one thing prints one line, and
one that prints many (`verify`, `list results`, `webhooks list`) prints one line each, so a script reads it line by
line (`jq -c`, or any JSON-lines reader). Field names are the API's.

```sh
verifyinboxes verify --json < addresses.txt | jq -r 'select(.safe_to_send) | .email'
```

Standard output holds only the results; progress, notes and errors go to standard error. `--quiet` drops the
progress and notes (errors are still printed). Colours are used only on a terminal, and never with `--no-color` or
when `NO_COLOR` is set.

## Exit codes

| Code | |
| --- | --- |
| 0 | Done. |
| 1 | An error: the API refused the request, or did not answer, or a wait ran out. |
| 2 | A mistake in the command line (an unknown command or flag, a missing argument, a wrong value). Nothing was sent. |
| 3 | `--fail-on invalid` found an invalid address, or `--fail-on unsafe` one not safe to send to. |
| 130 | Interrupted (Ctrl-C). |

## Errors

An error is printed on standard error as one line, with the API's error code, the HTTP status and the request id,
then what to do:

```console
$ verifyinboxes list status 019a...
error: job not found (not_found, HTTP 404, request id 019a7c2e-...)
```

Quote the request id when you write to support@verifyinboxes.com. When the failed command had started checks or a
list (a wait that ran out, a refused poll, Ctrl-C), the error names every job id, and how to pick each one up again:

```console
error: still running after 10m0s
The job 019a... carries on, and its credits are held. Wait for it again with:
  verifyinboxes list wait 019a...
```

Requests are retried by themselves when that is safe (no answer, `429`, `5xx`), with the same `Idempotency-Key`, so
a retried check or list is made and charged once.

## AI apps (MCP)

VerifyInboxes is also an MCP server, at `https://api.verifyinboxes.com/mcp`: an AI app that connects to remote MCP
servers uses that address and signs you in by itself. For an app that starts MCP servers as programs (stdio),
`verifyinboxes mcp` is a bridge to that server: the tools are the server's own, word for word. Each JSON-RPC message
the app writes is sent to the server, and each answer written back, one per line; nothing else is written on
standard output.

Sign in for it once (or set `VERIFYINBOXES_API_KEY` in the app's settings for an organisation's key):

```sh
verifyinboxes login --for mcp
```

Then add it to your app. If the app does not find `verifyinboxes`, give its full path (`command -v verifyinboxes`
prints it; on Windows, `where verifyinboxes`).

**Claude Code:**

```sh
claude mcp add verifyinboxes -- verifyinboxes mcp
```

**Claude Desktop** (`claude_desktop_config.json`, under Settings → Developer → Edit Config), **Cursor**
(`~/.cursor/mcp.json`) and **Windsurf** (`~/.codeium/windsurf/mcp_config.json`):

```json
{
  "mcpServers": {
    "verifyinboxes": { "command": "verifyinboxes", "args": ["mcp"] }
  }
}
```

**VS Code** (`.vscode/mcp.json`, or **MCP: Add Server** in the command palette):

```json
{
  "servers": {
    "verifyinboxes": { "type": "stdio", "command": "verifyinboxes", "args": ["mcp"] }
  }
}
```

**Codex** (`~/.codex/config.toml`):

```toml
[mcp_servers.verifyinboxes]
command = "verifyinboxes"
args = ["mcp"]
```

Without a credential, `verifyinboxes mcp` stops at once and says: `run "verifyinboxes login --for mcp" once, or set
VERIFYINBOXES_API_KEY`. Its own notes and errors go to standard error, which the app keeps in its MCP log.

## Configuration

| Variable | |
| --- | --- |
| `VERIFYINBOXES_API_KEY` | The credential to use, before anything saved. |
| `VERIFYINBOXES_BASE_URL` | The API's address (default `https://api.verifyinboxes.com`); `--base-url` beats it. What is saved is kept per address. |
| `VERIFYINBOXES_CONFIG_DIR` | Keep what you save in `credentials.json` in this directory, instead of the keychain. |
| `NO_COLOR` | No colours. |

Requests carry the `User-Agent` `verifyinboxes-cli/<version> verifyinboxes-go/<version>`.

## Versioning

The tool follows [semantic versioning](https://semver.org): until 1.0.0, a minor version may change a command or an
output; from 1.0.0 on, only a major one. The `--json` fields are the API's, which only adds fields: read the ones you
need and ignore the rest. Each release's notes say what changed.

## Security

See [SECURITY.md](SECURITY.md). The tool never prints a key or a token, takes none on its command line, and never
sends one anywhere but the API's address. The browser sign-in uses PKCE and a random `state` checked when the browser
comes back, and listens on `127.0.0.1` only, for that one answer.

## License

MIT: see [LICENSE](LICENSE).
