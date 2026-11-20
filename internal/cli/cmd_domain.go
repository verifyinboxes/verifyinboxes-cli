package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func (a *App) cmdDomain() *cobra.Command {
	return &cobra.Command{
		Use:     "domain <domain>",
		GroupID: "check",
		Short:   "Show what is known about a domain",
		Long: `Show a domain's mail provider and MX hosts, whether it accepts mail, whether it accepts every address
(catch-all), and the usual shape of its addresses. A lookup costs 1 credit when DNS answers.`,
		Example: `  verifyinboxes domain example.com`,
		Args:    exactArgs("domain"),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.client()
			if err != nil {
				return err
			}
			d, err := client.Domains.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(d)
			}
			hosts := make([]string, 0, len(d.MXHosts))
			for _, h := range d.MXHosts {
				hosts = append(hosts, fmt.Sprintf("%s (%d)", h.Host, h.Preference))
			}
			items := [][2]string{
				{"Domain", d.Domain},
				{"Provider", deref(d.Provider, "none")},
				{"Accepts mail", yesNo(d.AcceptsMail)},
				{"MX hosts", strings.Join(hosts, ", ")},
				{"Security gateway", yesNo(d.Gateway)},
			}
			catchAll := "not tested in the last 7 days"
			if d.CatchAll != nil {
				catchAll = yesNo(*d.CatchAll)
			}
			items = append(items, [2]string{"Catch-all", catchAll})
			if d.EmailPattern != nil {
				pattern := *d.EmailPattern
				if d.PatternConfidence != nil {
					pattern += fmt.Sprintf(" (%.0f%% of known addresses)", *d.PatternConfidence*100)
				}
				items = append(items, [2]string{"Address pattern", pattern})
			}
			if d.RegisteredAt != nil {
				items = append(items, [2]string{"Registered", *d.RegisteredAt})
			}
			pairs(a.Out, items)
			return nil
		},
	}
}

func deref(s *string, otherwise string) string {
	if s == nil {
		return otherwise
	}
	return *s
}
