package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type report struct {
	text     string // job log, plus the ::error:: annotation shown on the checks page
	markdown string // job summary, rendered on the checks page without opening the log
	comment  string // pull request comment, the only report seen without opening the job log
}

// commentMarker identifies the gate's own comment so a re-run edits it instead of
// posting another. It is invisible in the rendered comment.
const commentMarker = "<!-- clacheck:signature-request -->"

// Marshalled from Signature rather than written out by hand, so the entry a
// contributor is told to paste keeps whatever shape the gate parses back.
func entryJSON(p Principal, version, indent string, now time.Time) string {
	// Cannot fail: Signature is four scalar fields.
	b, _ := json.MarshalIndent(Signature{
		Login: p.Login,
		ID:    p.ID,
		Date:  now.UTC().Format(time.DateOnly),
		CLA:   version,
	}, indent, "  ")
	return indent + string(b)
}

// unsignedReport is what a contributor actually meets when the gate fails. It
// carries their entry already filled in, so signing is a copy and a commit rather
// than a hunt through documentation for a numeric id they have never needed.
//
// Only the opener's entry is offered: appendOnly accepts no other, so printing one
// for a co-author would be advice that fails on the next run.
func unsignedReport(cfg config, head *SignatureFile, missing []Principal, unknown []string, now time.Time) report {
	mine, others := splitByOpener(missing, cfg.opener.ID)
	claURL := fmt.Sprintf("%s/%s/blob/%s/CLA.md", cfg.serverURL, cfg.repo, cfg.baseRef)
	named := append(loginsOf(missing), unknown...)

	var text strings.Builder
	// The names carry a trailer address, which is the pull request's to choose, so
	// the annotation is encoded: a bare %0A in one would start a second command.
	fmt.Fprintf(&text, "::error::CLA %s not signed by: %s\n", head.CLAVersion, escapeAnnotation(strings.Join(named, ", ")))
	fmt.Fprintf(&text, "\nThe agreement: %s\n", claURL)
	if mine != nil {
		fmt.Fprintf(&text, "\nAdd this entry to signatures/cla.json, then commit and push — that\n")
		fmt.Fprintf(&text, "commit is your signature:\n\n")
		fmt.Fprintf(&text, "%s\n", entryJSON(*mine, head.CLAVersion, "  ", now))
	}
	for _, b := range trailerBlocks(others, unknown, verbatim) {
		fmt.Fprintf(&text, "\n%s\n", b)
	}

	marked := trailerBlocks(others, unknown, mdCode)
	return report{
		text:     text.String(),
		markdown: signMarkdown(claURL, head, mine, marked, now, false),
		comment:  commentMarker + "\n" + signMarkdown(claURL, head, mine, marked, now, true),
	}
}

// A trailer address is whatever the commit message said, so it reaches markdown
// only inside a code span, which renders it literally. The backtick that would
// end the span early is dropped rather than escaped: an address has no use for
// one, and a half-escaped span is worse than a missing character.
func mdCode(s string) string { return "`" + strings.ReplaceAll(s, "`", "") + "`" }

func verbatim(s string) string { return s }

// appendOnly will not take a co-author's signature from this pull request, and an
// assistant holds no copyright to license, so each block has to name the way out
// or the gate is red with none. quote renders the trailer address for whichever
// surface the blocks are bound for; a login needs none of it, since it comes back
// from the API rather than out of the commit.
func trailerBlocks(others []Principal, unknown []string, quote func(string) string) []string {
	var out []string
	if n := len(others); n > 0 {
		verb := "has work in this pull request and has not signed"
		if n > 1 {
			verb = "have work in this pull request and have not signed"
		}
		out = append(out, joinNames(loginsOf(others))+" "+verb+
			". Everyone signs in a pull request they opened themselves, so this one cannot sign for them.")
	}
	if n := len(unknown); n > 0 {
		quoted := make([]string, n)
		for i, u := range unknown {
			quoted[i] = quote(u)
		}
		lead := "A Co-authored-by trailer names "
		if n > 1 {
			lead = "Co-authored-by trailers name "
		}
		// The address never opens the line: one starting "::" is a workflow
		// command, and this line goes to the log as well.
		out = append(out, lead+joinNames(quoted)+
			", which the check cannot identify. Use the "+quote("<id>+<login>@users.noreply.github.com")+
			" address GitHub writes itself, or drop the trailer if it names an assistant.")
	}
	return out
}

// joinNames renders a list into the sentence around it: "a", "a and b",
// "a, b and c".
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func loginsOf(ps []Principal) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Login
	}
	return out
}

func splitByOpener(missing []Principal, openerID int64) (mine *Principal, others []Principal) {
	for i, p := range missing {
		if p.ID == openerID {
			mine = &missing[i]
			continue
		}
		others = append(others, p)
	}
	return mine, others
}

// Only the opener is mentioned: every other login can be invented by a
// Co-authored-by trailer, which would let a pull request notify anyone it names.
func signMarkdown(claURL string, head *SignatureFile, mine *Principal, blocks []string, now time.Time, mention bool) string {
	var md strings.Builder
	// The comment notifies the opener. With nothing for them to sign, a heading
	// demanding their signature reads as a demand they have already met.
	heading, tail := "## CLA signature required", ""
	if mine == nil {
		heading, tail = "## CLA check blocked", " Still outstanding:"
	}
	fmt.Fprintf(&md, "%s\n\n", heading)
	fmt.Fprintf(&md, "Thanks for contributing! Everyone with work in this pull request has to "+
		"sign the [Contributor License Agreement](%s) before it can merge.%s\n", claURL, tail)
	if mine != nil {
		name := mine.Login
		if mention {
			name = "@" + name
		}
		fmt.Fprintf(&md, "\n**%s** — add this to the `signatures` array in `signatures/cla.json`, "+
			"then commit and push. That commit is your signature.\n\n```json\n%s\n```\n",
			name, entryJSON(*mine, head.CLAVersion, "", now))
	}
	for _, b := range blocks {
		fmt.Fprintf(&md, "\n%s\n", b)
	}
	return md.String()
}

// The gate's two labels. They are mutually exclusive, so setting one clears the
// other; create them in the repository so they get a deliberate colour.
const (
	labelSigned   = "cla: signed"
	labelUnsigned = "cla: not signed"
)

// signedComment replaces a request comment once the signature lands, so a merged
// pull request is not left showing a demand that has already been met.
func signedComment(version string) string {
	return fmt.Sprintf("%s\nCLA %s signed — thanks!\n", commentMarker, version)
}

// Like problemComment, the reason stays in the log: it quotes a login out of the
// pull request's own file. The comment only has to get the contributor there.
func rejectedComment() string {
	return commentMarker + "\nThe change to `signatures/cla.json` was rejected. See the job log on the checks tab for what to fix.\n"
}

// The error itself is left to the log: it can quote a login out of the pull
// request's own file, which would break out of any markup used here.
func problemComment() string {
	return commentMarker + "\nThe CLA check did not finish. See the job log on the checks tab for what went wrong.\n"
}
