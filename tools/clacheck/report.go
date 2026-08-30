package main

import (
	"fmt"
	"strings"
	"time"
)

// placeholderName is what entryJSON hands out and what validate refuses back:
// a signature under it records agreement by nobody.
const placeholderName = "Your Name"

type report struct {
	text     string // job log, plus the ::error:: annotation shown on the checks page
	markdown string // job summary, rendered on the checks page without opening the log
}

func entryJSON(p Principal, version, indent string, now time.Time) string {
	return fmt.Sprintf(`%s{ "login": %q,
%s  "id":    %d,
%s  "name":  %q,
%s  "date":  %q,
%s  "cla":   %q }`,
		indent, p.Login, indent, p.ID, indent, placeholderName, indent, now.UTC().Format(time.DateOnly), indent, version)
}

// unsignedReport is what a contributor actually meets when the gate fails. It
// carries their entry already filled in, so signing is a copy and a commit rather
// than a hunt through documentation for a numeric id they have never needed.
func unsignedReport(cfg config, version string, missing []Principal, now time.Time) report {
	logins := make([]string, len(missing))
	for i, p := range missing {
		logins[i] = p.Login
	}
	claURL := fmt.Sprintf("%s/%s/blob/%s/CLA.md", cfg.serverURL, cfg.repo, cfg.baseRef)
	entries := plural(len(missing), "this entry", "these entries")

	var text strings.Builder
	fmt.Fprintf(&text, "::error::CLA %s not signed by: %s\n", version, strings.Join(logins, ", "))
	fmt.Fprintf(&text, "\nThanks for the contribution! One thing before this can merge.\n\n")
	fmt.Fprintf(&text, "Read the agreement: %s\n\n", claURL)
	fmt.Fprintf(&text, "If you agree, add %s to signatures/cla.json in this pull request:\n\n", entries)
	for _, p := range missing {
		fmt.Fprintf(&text, "%s\n\n", entryJSON(p, version, "  ", now))
	}
	fmt.Fprintf(&text, "Replace %q with your own name, then commit it and push — that commit is\n", placeholderName)
	fmt.Fprintf(&text, "your signature. You only do it once per CLA version.\n")

	var md strings.Builder
	fmt.Fprintf(&md, "## Signature required\n\n")
	fmt.Fprintf(&md, "Thanks for contributing! Before this can merge, everyone whose work is in this "+
		"pull request needs to have signed the [Contributor License Agreement](%s).\n\n", claURL)
	fmt.Fprintf(&md, "**Not signed yet:** %s\n\n", strings.Join(logins, ", "))
	fmt.Fprintf(&md, "### How to sign\n\n")
	fmt.Fprintf(&md, "Add %s to the `signatures` array in `signatures/cla.json`, replacing `%s`:\n\n", entries, placeholderName)
	fmt.Fprintf(&md, "```json\n")
	for i, p := range missing {
		if i > 0 {
			md.WriteString(",\n")
		}
		md.WriteString(entryJSON(p, version, "", now))
	}
	fmt.Fprintf(&md, "\n```\n\n")
	fmt.Fprintf(&md, "Commit that to this pull request and push. Your id is already filled in above — "+
		"it identifies you even if you later change your username.\n\n")
	fmt.Fprintf(&md, "You sign **once** per CLA version. It covers everything you contribute here afterwards.\n")

	return report{text: text.String(), markdown: md.String()}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
