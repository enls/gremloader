package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/enls/gremloader/internal/engine"
)

// jsonSchema is the version of the --json output. Scripts may depend on it: fields are only ever
// added; a removal or change of meaning bumps it.
const jsonSchema = 1

// jsonOut is the one object a command prints with --json.
type jsonOut struct {
	SchemaVersion int      `json:"schema_version"`
	Command       string   `json:"command"`
	ExitCode      int      `json:"exit_code"`
	Items         any      `json:"items,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
	Error         string   `json:"error,omitempty"`
}

func (c *call) writeJSON(o jsonOut) {
	o.SchemaVersion = jsonSchema
	enc := json.NewEncoder(c.io.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(o)
}

// jsonItem is a package result in --json output.
type jsonItem struct {
	Package string   `json:"package"`
	Action  string   `json:"action,omitempty"`
	Result  string   `json:"result"`
	From    string   `json:"from,omitempty"`
	To      string   `json:"to,omitempty"`
	Dir     string   `json:"dir,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Notes   []string `json:"notes,omitempty"`
	Signer  string   `json:"signer,omitempty"` // the workflow that built the download, when its attestation passed
	Error   string   `json:"error,omitempty"`
}

func jsonItems(items []*engine.Item) []jsonItem {
	out := make([]jsonItem, 0, len(items))
	for _, it := range items {
		j := jsonItem{Package: it.Package, Action: string(it.Action), Result: string(it.Result), From: it.From, To: it.To,
			Dir: it.Dir, Reason: it.Reason, Notes: it.Notes, Signer: it.Signer}
		if it.Err != nil {
			j.Error = it.Err.Error()
		}
		out = append(out, j)
	}
	return out
}

// report prints package results and returns code: as one JSON object, or as a table with each
// item's notes and error under it.
func (c *call) report(items []*engine.Item, warnings []string, code int) int {
	if c.g.json {
		c.writeJSON(jsonOut{Command: c.name, ExitCode: code, Items: jsonItems(items), Warnings: warnings})
		return code
	}
	for _, w := range warnings {
		say(c.io.Stderr, "warning: %s\n", w)
	}
	printItems(c.io.Stdout, items, c.g.quiet)
	return code
}

func printItems(w io.Writer, items []*engine.Item, quiet bool) {
	if len(items) == 0 {
		say(w, "nothing to do\n")
		return
	}
	// Columns are aligned over all items; notes and errors go on their own lines under each.
	nameW, verW := 0, 0
	for _, it := range items {
		nameW, verW = max(nameW, len(it.Package)), max(verW, len(versions(it)))
	}
	for _, it := range items {
		say(w, "%s\n", strings.TrimRight(fmt.Sprintf("%-*s  %-*s  %s", nameW, it.Package, verW, versions(it), status(it)), " "))
		if it.Err != nil {
			say(w, "  error: %s\n", indent(it.Err.Error()))
		}
		if !quiet {
			for _, n := range it.Notes {
				say(w, "  note: %s\n", indent(n))
			}
			if it.Signer != "" {
				say(w, "  attested: built by %s\n", it.Signer)
			}
		}
	}
}

// versions is "1.0.0 -> 2.0.0", or the one version there is.
func versions(it *engine.Item) string {
	switch {
	case it.From != "" && it.To != "" && it.From != it.To:
		return it.From + " -> " + it.To
	case it.To != "":
		return it.To
	default:
		return it.From
	}
}

// status is the item's outcome in a few words: the action still to do for a plan, the result
// once applied, and the reason when there is one.
func status(it *engine.Item) string {
	s := string(it.Result)
	switch it.Result {
	case engine.ResultPending:
		s = string(it.Action)
	case engine.ResultUnchanged:
		s = "up to date"
	case engine.ResultUpdated:
		// The result says only that the new files are in; the action says which way (JSON keeps both).
		switch it.Action {
		case engine.ActDowngrade:
			s = "downgraded"
		case engine.ActReinstall:
			s = "reinstalled"
		}
	}
	if it.Reason != "" {
		s += ": " + it.Reason
	}
	return s
}

// indent keeps a multi-line message under its item.
func indent(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}

// table prints rows with aligned columns; the first row is the header.
func table(w io.Writer, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		say(tw, "%s\n", strings.Join(r, "\t"))
	}
	_ = tw.Flush()
}
