package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Signature is one contributor's entry in signatures/cla.json. Identity is the
// numeric GitHub id: logins can be renamed and re-registered by someone else,
// which would silently transfer a signature.
type Signature struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Date  string `json:"date"`
	CLA   string `json:"cla"`
}

type SignatureFile struct {
	CLAVersion string      `json:"cla_version"`
	Signatures []Signature `json:"signatures"`
}

// Principal is anyone whose copyright can reach the repo through a pull request.
type Principal struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

func (p Principal) isBot() bool { return p.Type == "Bot" }

type Commit struct {
	SHA       string     `json:"sha"`
	Author    *Principal `json:"author"`
	Committer *Principal `json:"committer"`
	Commit    struct {
		Message string `json:"message"`
	} `json:"commit"`
}

// A version is echoed into the job log, where GitHub reads ::workflow:: commands
// line by line, so it must not carry a newline.
var versionRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validDate wants a real calendar day, not the shape of one: time.Parse is strict
// about both, so it rejects 2026-2-3 and 2026-02-30 alike.
func validDate(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// validate rejects a signature file that records agreement to nothing: an entry
// with no id names nobody. The version is held to versionRe for the reason given
// at its declaration — report.go interpolates it into an annotation unescaped.
func (f *SignatureFile) validate() error {
	if f.CLAVersion == "" {
		return errors.New("cla_version is missing or empty")
	}
	if !versionRe.MatchString(f.CLAVersion) {
		return fmt.Errorf("cla_version %q must be one line of letters, digits, dot, dash or underscore", f.CLAVersion)
	}
	if f.Signatures == nil {
		return errors.New("signatures is missing")
	}
	seen := make(map[string]int, len(f.Signatures))
	for i, s := range f.Signatures {
		var problem string
		switch {
		case s.Login == "":
			problem = "login is empty"
		case s.ID == 0:
			problem = "id is missing or zero"
		case !validDate(s.Date):
			problem = "date is not a real YYYY-MM-DD date"
		case !versionRe.MatchString(s.CLA):
			problem = "cla is missing or malformed"
		}
		if problem != "" {
			return fmt.Errorf("signatures[%d] (%s): %s", i, s.Login, problem)
		}
		// Keyed by version too: a version bump is signed by adding an entry, so
		// the same id legitimately appears once per version it has agreed to.
		key := fmt.Sprintf("%d/%s", s.ID, s.CLA)
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("signatures[%d] repeats the id and version already used by signatures[%d]", i, prev)
		}
		seen[key] = i
	}
	return nil
}

// signed wants an entry at the file's current version. An entry left at a
// superseded version reads as unsigned, so a version bump hands the contributor
// the ordinary sign-me report instead of failing the file as a whole.
func (f *SignatureFile) signed(id int64) bool {
	return slices.ContainsFunc(f.Signatures, func(s Signature) bool {
		return s.ID == id && s.CLA == f.CLAVersion
	})
}

// webFlowID is GitHub's committer for web-UI merges and applied suggestions,
// matched on the id: a login is resolved from a commit email the contributor
// chooses, so excluding by name would let anyone drop out of the check.
const webFlowID int64 = 19864447

// principals reads the author and the committer of every commit. Both are
// self-asserted — `--author=` and GIT_COMMITTER_EMAIL are free to set — so
// neither proves who pushed; they are collected because each names a distinct
// copyright holder. The opener comes from the webhook and cannot be forged.
func principals(commits []Commit, opener Principal) (found []Principal, unlinked []string) {
	add := func(p *Principal) {
		if p != nil && p.ID != webFlowID {
			found = append(found, *p)
		}
	}
	for _, c := range commits {
		if c.Author == nil || c.Committer == nil {
			unlinked = append(unlinked, c.SHA)
			continue
		}
		add(c.Author)
		add(c.Committer)
	}
	return append(found, opener), unlinked
}

var coauthorRe = regexp.MustCompile(`(?mi)^[ \t]*co-authored-by:[^<>\n]*<([^>\n]+)>`)

// coauthorEmails pulls Co-authored-by trailers out of the commit messages. A
// co-author holds copyright and is invisible to the commits endpoint, so without
// this a pair-written commit licenses only half of what it contains.
func coauthorEmails(commits []Commit) []string {
	var out []string
	for _, c := range commits {
		for _, m := range coauthorRe.FindAllStringSubmatch(c.Commit.Message, -1) {
			// A lone CR ends a line for both the Actions log and CommonMark, so
			// leaving one in would let a trailer forge a workflow command and
			// break out of the code span the report renders it in.
			out = append(out, strings.ToLower(strings.TrimSpace(strings.ReplaceAll(m[1], "\r", ""))))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// An assistant holds no copyright, so a trailer naming one names no principal and
// there is nothing for it to sign. Blocking would make every contributor using one
// rewrite their branch over a line that licenses nothing. Matched whole and
// lowercased, which is how coauthorEmails hands an address over; add to this list
// rather than loosening the check, so an address that might be a person still stops
// the gate.
var assistantEmails = []string{
	"noreply@anthropic.com", // Claude Code's default Co-authored-by trailer
}

func isAssistant(email string) bool { return slices.Contains(assistantEmails, email) }

var noreplyRe = regexp.MustCompile(`^(?:\d+\+)?([A-Za-z0-9-]+(?:\[bot\])?)@users\.noreply\.github\.com$`)

// noreplyLogin pulls the login out of a GitHub noreply address. The id in the
// <id>+<login> form is discarded rather than read: the whole address comes from a
// commit message, so an id taken on trust there would let a pull request sign for
// anyone it named.
func noreplyLogin(email string) string {
	if m := noreplyRe.FindStringSubmatch(email); m != nil {
		return m[1]
	}
	return ""
}

// appendOnly keeps existing entries immutable and takes only the opener's own
// signature. The opener is the one principal that cannot be forged: an author, a
// committer and a trailer are all self-asserted, so accepting a signature for any
// principal would let a pull request sign for anyone it named.
//
// inForce comes from the base branch tip, not from base: a branch that predates a
// version bump would otherwise sign the retired version and pass.
func appendOnly(base, head *SignatureFile, signer Principal, inForce string) error {
	if head.CLAVersion != inForce {
		return fmt.Errorf("cla_version is %q on the base branch but %q here; it is set on the base branch, not by a contribution, so merge the base branch if this one is behind",
			inForce, head.CLAVersion)
	}
	for _, b := range base.Signatures {
		if !slices.Contains(head.Signatures, b) {
			return fmt.Errorf("this pull request edits or removes the signature of %q; signatures/cla.json is append-only", b.Login)
		}
	}
	for _, h := range head.Signatures {
		switch {
		case slices.Contains(base.Signatures, h):
		case h.ID != signer.ID:
			return fmt.Errorf("this pull request adds a signature for %q, who did not open it; you may only sign for yourself, so a co-author signs in a pull request of their own", h.Login)
		// Signing matches on the id, so a mismatched login would stand in the
		// record as a signature by whoever it names.
		case !strings.EqualFold(h.Login, signer.Login):
			return fmt.Errorf("this signature records id %d under the login %q, but that id belongs to %q", h.ID, h.Login, signer.Login)
		// You sign what is in force. A new entry at a retired version records an
		// agreement never given, and signed() reads it as unsigned anyway.
		case h.CLA != head.CLAVersion:
			return fmt.Errorf("this signature is recorded against CLA %q, but %q is the version in force; sign the current one",
				h.CLA, head.CLAVersion)
		}
	}
	return nil
}

// unsigned returns the principals that still owe a signature, deduplicated by id
// and with bots dropped: a machine-authored commit carries no human authorship to
// license. Every id here was resolved against the API, so a login cannot be
// shaped to look like a bot and slip through.
func unsigned(head *SignatureFile, ps []Principal) (missing, checked []Principal) {
	seen := make(map[int64]bool, len(ps))
	for _, p := range ps {
		if p.isBot() || p.ID == 0 || seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		checked = append(checked, p)
		if !head.signed(p.ID) {
			missing = append(missing, p)
		}
	}
	slices.SortFunc(checked, func(a, b Principal) int { return strings.Compare(a.Login, b.Login) })
	slices.SortFunc(missing, func(a, b Principal) int { return strings.Compare(a.Login, b.Login) })
	return missing, checked
}
