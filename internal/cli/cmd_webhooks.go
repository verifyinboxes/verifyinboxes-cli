package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/verifyinboxes/verifyinboxes-go"
)

func (a *App) cmdWebhooks() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "webhooks",
		GroupID: "setup",
		Short:   "Where the API sends events",
		Long: `Webhooks: the addresses the API sends events to (job.completed when a list finishes,
address.contradicted when a verdict you bought is overturned). Each delivery is signed with the webhook's secret,
shown once when it is made.`,
	}
	group(cmd)
	cmd.AddCommand(&cobra.Command{
		Use:     "list",
		Short:   "Show the webhooks",
		Example: `  verifyinboxes webhooks list`,
		Args:    exactArgs(),
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			hooks, err := client.Webhooks.List(c.Context())
			if err != nil {
				return err
			}
			if a.jsonOut {
				for _, h := range hooks {
					if err := a.printJSON(h); err != nil {
						return err
					}
				}
				return nil
			}
			if len(hooks) == 0 {
				fmt.Fprintln(a.Out, "No webhooks.")
				return nil
			}
			rows := make([][]string, 0, len(hooks))
			for _, h := range hooks {
				rows = append(rows, []string{h.ID, h.URL, strings.Join(h.Events, ","), h.CreatedAt})
			}
			table(a.Out, []string{"ID", "URL", "EVENTS", "CREATED"}, rows, nil)
			return nil
		},
	})
	var events []string
	add := &cobra.Command{
		Use:   "add <url>",
		Short: "Make a webhook; its secret is shown once",
		Example: `  verifyinboxes webhooks add https://example.com/hooks/verifyinboxes
  verifyinboxes webhooks add https://example.com/hooks/lists --event job.completed`,
		Args: exactArgs("url"),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			hook, err := client.Webhooks.Create(c.Context(), &verifyinboxes.WebhookParams{URL: args[0], Events: events})
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(hook)
			}
			fmt.Fprintf(a.Out, "Made webhook %s for %s (%s).\n", clean(hook.ID), clean(hook.URL), clean(strings.Join(hook.Events, ", ")))
			fmt.Fprintf(a.Out, "Its signing secret, shown only now (store it now):\n\n  %s\n", clean(hook.Secret))
			return nil
		},
	}
	add.Flags().StringArrayVar(&events, "event", nil, "an `event` to send: job.completed or address.contradicted (default both; repeat it for more)")
	cmd.AddCommand(add)
	cmd.AddCommand(&cobra.Command{
		Use:     "delete <id>",
		Short:   "Delete a webhook",
		Example: `  verifyinboxes webhooks delete 019a...`,
		Args:    exactArgs("id"),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			if err := client.Webhooks.Delete(c.Context(), args[0]); err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(map[string]any{"id": args[0], "deleted": true})
			}
			fmt.Fprintf(a.Out, "Deleted webhook %s.\n", args[0])
			return nil
		},
	})
	return cmd
}
