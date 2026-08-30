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
	Name  string `json:"name"`
	Date  string `json:"date"`
	CLA   string `json:"cla"`
}

type SignatureFile struct {
	Comment    string      `json:"_comment,omitempty"`
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

// validate rejects a signature file that records agreement to nothing: a missing
// id, or the placeholder name the report hands out left unedited.
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
		case s.Name == "":
			problem = "name is empty"
		case s.Name == placeholderName:
			problem = "name is still the placeholder; put your own name in"
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
			out = append(out, strings.ToLower(strings.TrimSpace(m[1])))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

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

// appendOnly enforces what the file's own comment promises: existing entries are
// immutable, and a pull request may only sign for someone who authored part of it.
// Without the second half, anyone could sign on a stranger's behalf.
func appendOnly(base, head *SignatureFile, prIDs map[int64]bool) error {
	if base.CLAVersion != "" && head.CLAVersion != base.CLAVersion {
		return fmt.Errorf("this pull request changes cla_version from %q to %q; the version is set on the base branch, not in a contribution",
			base.CLAVersion, head.CLAVersion)
	}
	for _, b := range base.Signatures {
		if !slices.Contains(head.Signatures, b) {
			return fmt.Errorf("this pull request edits or removes the signature of %q; signatures/cla.json is append-only", b.Login)
		}
	}
	for _, h := range head.Signatures {
		if slices.Contains(base.Signatures, h) || prIDs[h.ID] {
			continue
		}
		return fmt.Errorf("this pull request signs for %q, who authored none of its commits; you may only sign for yourself", h.Login)
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
