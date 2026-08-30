package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// placeholderName is what entryJSON hands out and what validate refuses back:
// a signature under it records agreement by nobody.
const placeholderName = "Your Name"

type report struct {
	text     string // job log, plus the ::error:: annotation shown on the checks page
	markdown string // job summary, rendered on the checks page without opening the log
	comment  string // pull request comment, the only report seen without opening the job log
}

// commentMarker identifies the gate's own comment so a re-run edits it instead of
// posting another. It is invisible in the rendered comment.
const commentMarker = "<!-- clacheck:signature-request -->"

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
func unsignedReport(cfg config, head *SignatureFile, missing []Principal, now time.Time) report {
	version := head.CLAVersion
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

	whole, ok := signedFile(head, missing, now)
	return report{
		text:     text.String(),
		markdown: signMarkdown(claURL, head, missing, now, whole, ok, 0),
		comment:  commentMarker + "\n" + signMarkdown(claURL, head, missing, now, whole, ok, cfg.opener.ID),
	}
}

// Only mentionID is mentioned: every other login can be invented by a
// Co-authored-by trailer, which would let a pull request notify anyone it names.
func signMarkdown(claURL string, head *SignatureFile, missing []Principal, now time.Time, whole string, wholeFits bool, mentionID int64) string {
	version := head.CLAVersion
	named := make([]string, len(missing))
	for i, p := range missing {
		if named[i] = p.Login; p.ID == mentionID {
			named[i] = "@" + p.Login
		}
	}
	entries := plural(len(missing), "this entry", "these entries")

	var md strings.Builder
	fmt.Fprintf(&md, "## Signature required\n\n")
	fmt.Fprintf(&md, "Thanks for contributing! Before this can merge, everyone whose work is in this "+
		"pull request needs to have signed the [Contributor License Agreement](%s).\n\n", claURL)
	fmt.Fprintf(&md, "**Not signed yet:** %s\n\n", strings.Join(named, ", "))
	fmt.Fprintf(&md, "### How to sign\n\n")
	if wholeFits {
		fmt.Fprintf(&md, "Copy this over the whole of `signatures/cla.json`, replacing `%s` with your own:\n\n", placeholderName)
		fmt.Fprintf(&md, "```json\n%s```\n\n", whole)
	} else {
		fmt.Fprintf(&md, "Add %s to the `signatures` array in `signatures/cla.json`, %s:\n\n", entries,
			plural(len(missing), "replacing `"+placeholderName+"` with your own name",
				"replacing each `"+placeholderName+"` with that person's own name"))
		fmt.Fprintf(&md, "```json\n")
		for i, p := range missing {
			if i > 0 {
				md.WriteString(",\n")
			}
			md.WriteString(entryJSON(p, version, "", now))
		}
		fmt.Fprintf(&md, "\n```\n\n")
	}
	fmt.Fprintf(&md, "Commit that to this pull request and push. Your id is already filled in above — "+
		"it identifies you even if you later change your username.\n\n")
	fmt.Fprintf(&md, "You sign **once** per CLA version. It covers everything you contribute here afterwards.\n")

	return md.String()
}

// fullFileMaxSignatures caps the paste-the-whole-file form. Past it the comment
// would be longer than the change it asks for, so the entry is shown alone.
const fullFileMaxSignatures = 20

// Existing entries are reproduced verbatim, so pasting it is append-only. Single
// signer only: of two placeholders, whoever pastes fills in one and validate
// rejects the other on the next run.
func signedFile(head *SignatureFile, missing []Principal, now time.Time) (string, bool) {
	if len(missing) != 1 || len(head.Signatures) > fullFileMaxSignatures {
		return "", false
	}
	out := *head
	out.Signatures = slices.Clone(head.Signatures)
	for _, p := range missing {
		out.Signatures = append(out.Signatures, Signature{
			Login: p.Login,
			ID:    p.ID,
			Name:  placeholderName,
			Date:  now.UTC().Format(time.DateOnly),
			CLA:   head.CLAVersion,
		})
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", false
	}
	return string(b) + "\n", true
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

// The error itself is left to the log: it can quote a login out of the pull
// request's own file, which would break out of any markup used here.
func problemComment() string {
	return commentMarker + "\nThe CLA check did not finish. See the job log on the checks tab for what went wrong.\n"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
