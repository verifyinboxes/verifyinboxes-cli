package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/verifyinboxes/verifyinboxes-go"
)

func (a *App) cmdSuppressions() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "suppressions",
		GroupID: "setup",
		Short:   "The addresses and domains never to check",
		Long: `The suppression list: addresses and domains never to check. A check of one is answered invalid (reason
suppressed) and costs nothing. Addresses are kept only as a hash, so only their number is shown.`,
	}
	group(cmd)
	cmd.AddCommand(&cobra.Command{
		Use:     "list",
		Short:   "Show the domains on the list, and how many addresses",
		Example: `  verifyinboxes suppressions list`,
		Args:    exactArgs(),
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			list, err := client.Suppressions.List(c.Context())
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(list)
			}
			domains := strings.Join(list.Domains, ", ")
			if domains == "" {
				domains = "none"
			}
			pairs(a.Out, [][2]string{{"Domains", domains}, {"Addresses", fmt.Sprintf("%d (kept only as a hash)", list.AddressCount)}})
			return nil
		},
	})
	var emails, domains []string
	add := &cobra.Command{
		Use:   "add --email <address>... --domain <domain>...",
		Short: "Put addresses and domains on the list",
		Example: `  verifyinboxes suppressions add --email ann@example.com
  verifyinboxes suppressions add --domain example.org --domain example.net`,
		Args: exactArgs(),
		RunE: func(c *cobra.Command, _ []string) error {
			if len(emails) == 0 && len(domains) == 0 {
				return usageError("give --email, --domain or both", "See `verifyinboxes suppressions add --help`.")
			}
			client, err := a.client()
			if err != nil {
				return err
			}
			added, err := client.Suppressions.Add(c.Context(), &verifyinboxes.SuppressionParams{Emails: emails, Domains: domains})
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(map[string]int{"added": added})
			}
			fmt.Fprintf(a.Out, "Added %d to the suppression list.\n", added)
			return nil
		},
	}
	add.Flags().StringArrayVar(&emails, "email", nil, "an `address` to suppress (repeat it for more)")
	add.Flags().StringArrayVar(&domains, "domain", nil, "a `domain` to suppress (repeat it for more)")
	cmd.AddCommand(add)
	return cmd
}
