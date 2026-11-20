// Command verifyinboxes is the command-line tool for the VerifyInboxes API.
package main

import (
	"os"

	"github.com/verifyinboxes/verifyinboxes-cli/internal/cli"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	os.Exit(cli.Main(cli.BuildInfo{Version: version, Commit: commit, Date: date}))
}
