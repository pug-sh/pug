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
