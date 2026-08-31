// The signer records a signature on behalf of someone who asked for it in a pull
// request comment.
//
// A comment's author is the one identity in this flow GitHub attests. A commit's
// author, its committer and a Co-authored-by trailer are all self-asserted, which
// is exactly why the checker refuses to take a signature from any of them; a
// comment carries no such doubt, so it can execute the agreement where those
// cannot. The signature is still written as a commit, authored as the
// contributor, so the record lives in git history under the identity that agreed.
package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
)

// signWorkflowFile is the checker's workflow, re-run once a signature lands so the
// pull request's own required check turns green rather than a second check
// merely agreeing with it.
const signWorkflowFile = "cla.yaml"

type signConfig struct {
	repo      string
	pr        int
	commenter Principal
	token     string
}

func loadSignConfig() (signConfig, error) {
	c := signConfig{
		repo:  os.Getenv("GITHUB_REPOSITORY"),
		token: env("GH_TOKEN", os.Getenv("GITHUB_TOKEN")),
	}
	var err error
	if c.pr, err = strconv.Atoi(os.Getenv("PR_NUMBER")); err != nil {
		return c, fmt.Errorf("PR_NUMBER: %w", err)
	}
	id, err := strconv.ParseInt(os.Getenv("COMMENTER_ID"), 10, 64)
	if err != nil {
		return c, fmt.Errorf("COMMENTER_ID: %w", err)
	}
	c.commenter = Principal{ID: id, Login: os.Getenv("COMMENTER_LOGIN"), Type: env("COMMENTER_TYPE", "User")}
	// Each of these weakens the signer if it is missing rather than wrong: a zero
	// id names nobody to sign for, and an absent token downgrades the run to the
	// unauthenticated rate limit before failing the write outright.
	switch {
	case c.repo == "":
		return c, errors.New("GITHUB_REPOSITORY is empty")
	case c.token == "":
		return c, errors.New("GH_TOKEN is empty")
	case c.commenter.ID == 0:
		return c, errors.New("COMMENTER_ID is zero")
	case c.commenter.Login == "":
		return c, errors.New("COMMENTER_LOGIN is empty")
	}
	return c, nil
}

// Refusals a contributor can meet. Each is reported verbatim in the reply, so
// whichever one fires is the whole of what they are told.
var (
	errNotAPrincipal = errors.New("you have no commits, and no co-author trailer, in this pull request")
	errAlreadySigned = errors.New("you have already signed the version in force")
	errBotCommenter  = errors.New("a bot holds no copyright, so it has nothing to license")
)

// maySign decides whether this comment records a signature, and when it does not,
// which reason the contributor is told.
//
// commenter is GitHub-attested, so its identity is not in question. people is
// every principal on the pull request: the opener, every commit author and
// committer, and every resolved Co-authored-by trailer. head is the pull
// request's own signature file and onBase the base branch's; a signature in
// either one already covers this version. version is the one in force.
//
// More than one reason can be true at once — a bot that is not a principal and
// has somehow signed hits three — so the order of the checks is the order of the
// contributor's experience: whichever fires first is the only message they read.
//
// Returns nil to accept.
func maySign(commenter Principal, people []Principal, head, onBase *SignatureFile, version string) error {
	// A bot first. It is the one refusal that holds whatever the pull request
	// says, and telling a machine which humans have signed is noise nobody reads.
	if commenter.isBot() {
		return errBotCommenter
	}
	// Before principal-hood, deliberately. Someone who signed on an earlier pull
	// request and comments /sign on a colleague's would otherwise be told they
	// have no work here — true, and it sends them hunting for a problem they do
	// not have. Both files are searched at the version in force, never at their
	// own: onBase can declare a different one mid-bump.
	if head.signedAt(commenter.ID, version) || onBase.signedAt(commenter.ID, version) {
		return errAlreadySigned
	}
	// The only security-relevant check, and the one never to soften: without it
	// the file fills with entries from people who have never contributed.
	if !slices.ContainsFunc(people, func(p Principal) bool { return p.ID == commenter.ID }) {
		return errNotAPrincipal
	}
	return nil
}
