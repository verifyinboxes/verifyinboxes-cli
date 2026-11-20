package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func (a *App) cmdAccount() *cobra.Command {
	return &cobra.Command{
		Use:     "account",
		GroupID: "setup",
		Short:   "Show the organisation, its balance and its settings",
		Long:    `Show the organisation the credential acts for: its name, its balance in credits and its settings.`,
		Example: `  verifyinboxes account
  verifyinboxes account --json`,
		Args: exactArgs(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			account, err := client.Account.Get(cmd.Context())
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(account)
			}
			pairs(a.Out, [][2]string{
				{"Organisation", account.Name},
				{"Balance", fmt.Sprintf("%d credits", account.Balance)},
				{"Safe to send", fmt.Sprintf("valid, or catch_all with a score of %g or more", account.SafeToSendThreshold)},
				{"Feedback counts", yesNo(account.Reviewed)},
				{"Test emails", yesNo(account.TestEmails)},
			})
			return nil
		},
	}
}
