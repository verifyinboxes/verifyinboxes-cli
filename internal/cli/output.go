package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/verifyinboxes/verifyinboxes-go"
)

// printJSON writes v to standard output as one line of JSON.
func (a *App) printJSON(v any) error {
	enc := json.NewEncoder(a.Out)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// colour is whether standard output gets colours.
func (a *App) colour() bool {
	return a.OutTTY && !a.noColor && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

// paint colours a verdict on a terminal.
func (a *App) paint(verdict string) string {
	if !a.colour() {
		return verdict
	}
	code := ""
	switch verdictName(verdict) {
	case "valid":
		code = "32"
	case "invalid":
		code = "31"
	case "catch_all", "risky":
		code = "33"
	default:
		return verdict
	}
	return "\x1b[" + code + "m" + verdict + "\x1b[0m"
}

func verdictName(cell string) string { return strings.TrimSpace(cell) }

// clean is text from the API made safe to print on a terminal: control characters (C0 and C1, an escape sequence's
// ESC among them), the bidirectional controls, the zero-width space and the byte order mark dropped, so that what a
// server sends can neither drive the terminal nor make a line read other than it is. The other format characters
// stay: the zero-width joiner and non-joiner, the soft hyphen and the emoji tags are how Persian, Indic and other
// text is written, in names and in internationalised addresses.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || misleading(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
}

// misleading is a format character that can make text read other than it is: a bidirectional control (an
// embedding, override or isolate, a left-to-right, right-to-left or Arabic letter mark), the zero-width space, the
// byte order mark.
func misleading(r rune) bool {
	switch {
	case r >= '\u202a' && r <= '\u202e', r >= '\u2066' && r <= '\u2069':
		return true
	}
	switch r {
	case '\u200e', '\u200f', '\u061c', '\u200b', '\ufeff':
		return true
	}
	return false
}

// pairs prints "Label  value" lines with the values aligned.
func pairs(w io.Writer, items [][2]string) {
	width := 0
	for _, item := range items {
		width = max(width, utf8.RuneCountInString(item[0]))
	}
	for _, item := range items {
		fmt.Fprintf(w, "%s%s  %s\n", item[0], strings.Repeat(" ", width-utf8.RuneCountInString(item[0])), clean(item[1]))
	}
}

// table prints rows under headings in aligned columns; paint, when set, colours a column's cells after they are
// padded.
func table(w io.Writer, head []string, rows [][]string, paint map[int]func(string) string) {
	widths := make([]int, len(head))
	for i, h := range head {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, row := range rows {
		for i := range row {
			row[i] = clean(row[i])
			widths[i] = max(widths[i], utf8.RuneCountInString(row[i]))
		}
	}
	line := func(cells []string, painted bool) {
		var b strings.Builder
		for i, cell := range cells {
			padded := cell
			if i < len(cells)-1 {
				padded += strings.Repeat(" ", widths[i]-utf8.RuneCountInString(cell)) + "  "
			}
			if p := paint[i]; painted && p != nil {
				padded = p(padded)
			}
			b.WriteString(padded)
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	line(head, false)
	for _, row := range rows {
		line(row, true)
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// printResults prints checked addresses: a table, or one Result per line with --json.
func (a *App) printResults(results []verifyinboxes.Result) error {
	if a.jsonOut {
		for _, r := range results {
			if err := a.printJSON(r); err != nil {
				return err
			}
		}
		return nil
	}
	rows := make([][]string, 0, len(results))
	for _, r := range results {
		reason := r.Reason
		if r.DidYouMean != "" {
			reason += " (did you mean " + r.DidYouMean + "?)"
		}
		rows = append(rows, []string{r.Email, string(r.Verdict), yesNo(r.SafeToSend), reason})
	}
	table(a.Out, []string{"EMAIL", "VERDICT", "SAFE TO SEND", "REASON"}, rows, map[int]func(string) string{1: a.paint})
	return nil
}

// printJob prints a job as it is now: pairs, or the Job with --json.
func (a *App) printJob(job *verifyinboxes.Job) error {
	if a.jsonOut {
		return a.printJSON(job)
	}
	label := "List"
	switch job.Kind {
	case "single":
		label = "Check"
	case "resolve":
		label = "Resolution"
	}
	items := [][2]string{{label, job.ID}, {"Status", string(job.Status)}, {"Checked", fmt.Sprintf("%d of %d", job.Done, job.Total)}}
	if c := job.Counts; c != nil {
		items = append(items, [2]string{"Verdicts", fmt.Sprintf("valid %d, invalid %d, catch_all %d, risky %d, unknown %d",
			c.Valid, c.Invalid, c.CatchAll, c.Risky, c.Unknown)})
	}
	items = append(items, [2]string{"Started", job.CreatedAt})
	if job.FinishedAt != nil {
		items = append(items, [2]string{"Finished", *job.FinishedAt})
	}
	if job.Purged {
		items = append(items, [2]string{"Results", "deleted (30 days after the job finished)"})
	}
	pairs(a.Out, items)
	if job.Result != nil {
		fmt.Fprintln(a.Out)
		return a.printResults([]verifyinboxes.Result{*job.Result})
	}
	return nil
}
