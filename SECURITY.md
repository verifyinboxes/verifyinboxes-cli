# Security policy

## Reporting a vulnerability

Report it privately: use GitHub's private vulnerability reporting on this repository (**Security** → **Report a
vulnerability**), or email **support@verifyinboxes.com** with what you found and how to reproduce it. Please do not
open a public issue for a security problem.

We will confirm that we have your report, tell you what we find, and say when a fix is released.

## Supported versions

Fixes go into the latest release.

## Keys, tokens and sign-ins

Never put an API key, a personal access token or a webhook secret in an issue, a pull request or a log you share.
The tool never prints a key or a token, takes none on its command line, and keeps what you save in your system's
keychain, or, where there is none, in a file only you can read. If a key has been exposed, an organisation admin
revokes it in the VerifyInboxes app under **Developers** → **API keys** and makes a new one; `verifyinboxes logout`
ends this computer's sign-ins, and any sign-in can be ended in the app under **Connected apps**.
