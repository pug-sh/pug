package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLoadSignConfigReadsTheCommenter(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "pug-sh/pug")
	t.Setenv("PR_NUMBER", "107")
	t.Setenv("COMMENTER_ID", "1")
	t.Setenv("COMMENTER_LOGIN", "alice")
	t.Setenv("COMMENTER_TYPE", "User")
	t.Setenv("GH_TOKEN", "token")

	cfg, err := loadSignConfig()
	if err != nil {
		t.Fatalf("loadSignConfig: %v", err)
	}
	if cfg.commenter.ID != 1 || cfg.commenter.Login != "alice" {
		t.Errorf("commenter = %+v, want alice/1", cfg.commenter)
	}
	if cfg.pr != 107 {
		t.Errorf("pr = %d, want 107", cfg.pr)
	}
	if cfg.repo != "pug-sh/pug" {
		t.Errorf("repo = %q, want pug-sh/pug", cfg.repo)
	}
}

// Every one of these weakens the signer if it is missing rather than wrong: a
// zero id names nobody, and an empty token downgrades the run to the
// unauthenticated rate limit and then fails the write with a 401.
func TestLoadSignConfigRejectsWhatItCannotTrust(t *testing.T) {
	base := map[string]string{
		"GITHUB_REPOSITORY": "pug-sh/pug",
		"PR_NUMBER":         "107",
		"COMMENTER_ID":      "1",
		"COMMENTER_LOGIN":   "alice",
		"COMMENTER_TYPE":    "User",
		"GH_TOKEN":          "token",
	}
	for _, blank := range []string{"GITHUB_REPOSITORY", "COMMENTER_LOGIN", "GH_TOKEN"} {
		t.Run(blank, func(t *testing.T) {
			for k, v := range base {
				t.Setenv(k, v)
			}
			t.Setenv("GITHUB_TOKEN", "")
			t.Setenv(blank, "")
			if _, err := loadSignConfig(); err == nil {
				t.Fatalf("loadSignConfig accepted an empty %s", blank)
			}
		})
	}

	t.Run("zero commenter id", func(t *testing.T) {
		for k, v := range base {
			t.Setenv(k, v)
		}
		t.Setenv("COMMENTER_ID", "0")
		if _, err := loadSignConfig(); err == nil {
			t.Fatal("loadSignConfig accepted a zero commenter id")
		}
	})
}

func TestMaySignRefusesANonPrincipal(t *testing.T) {
	people := []Principal{{ID: 876188, Login: "poluruprvn", Type: "User"}}
	head := &SignatureFile{CLAVersion: "v1"}
	onBase := &SignatureFile{CLAVersion: "v1"}

	err := maySign(Principal{ID: 999, Login: "carol", Type: "User"}, people, head, onBase, "v1")
	if !errors.Is(err, errNotAPrincipal) {
		t.Fatalf("maySign for a stranger = %v, want errNotAPrincipal", err)
	}
}

// The whole point of signing by comment: a co-author can sign in the pull request
// they co-wrote instead of opening a throwaway one of their own.
func TestMaySignAcceptsACoAuthor(t *testing.T) {
	people := []Principal{
		{ID: 876188, Login: "poluruprvn", Type: "User"},
		{ID: 1, Login: "alice", Type: "User"},
	}
	head := &SignatureFile{CLAVersion: "v1"}
	onBase := &SignatureFile{CLAVersion: "v1"}

	if err := maySign(Principal{ID: 1, Login: "alice", Type: "User"}, people, head, onBase, "v1"); err != nil {
		t.Fatalf("maySign for a co-author = %v, want nil", err)
	}
}

func TestMaySignRefusesSomeoneAlreadySignedOnTheBaseBranch(t *testing.T) {
	people := []Principal{{ID: 1, Login: "alice", Type: "User"}}
	head := &SignatureFile{CLAVersion: "v1"}
	onBase := &SignatureFile{CLAVersion: "v1", Signatures: []Signature{
		{Login: "alice", ID: 1, Date: "2026-09-01", CLA: "v1"},
	}}

	err := maySign(Principal{ID: 1, Login: "alice", Type: "User"}, people, head, onBase, "v1")
	if !errors.Is(err, errAlreadySigned) {
		t.Fatalf("maySign for a signed contributor = %v, want errAlreadySigned", err)
	}
}

// A signature already in the pull request's own head counts too. Writing a second
// one would put the id in the file twice once the branch merges the base, and
// validate() rejects a repeated id and version outright — failing the gate for
// the crime of signing enthusiastically.
func TestMaySignRefusesSomeoneAlreadySignedInHead(t *testing.T) {
	people := []Principal{{ID: 1, Login: "alice", Type: "User"}}
	head := &SignatureFile{CLAVersion: "v1", Signatures: []Signature{
		{Login: "alice", ID: 1, Date: "2026-09-01", CLA: "v1"},
	}}
	onBase := &SignatureFile{CLAVersion: "v1"}

	err := maySign(Principal{ID: 1, Login: "alice", Type: "User"}, people, head, onBase, "v1")
	if !errors.Is(err, errAlreadySigned) {
		t.Fatalf("maySign for a contributor signed in head = %v, want errAlreadySigned", err)
	}
}

// A signature at a retired version is not a signature at the one in force.
func TestMaySignAcceptsSomeoneSignedOnlyAtAnOlderVersion(t *testing.T) {
	people := []Principal{{ID: 1, Login: "alice", Type: "User"}}
	head := &SignatureFile{CLAVersion: "v2"}
	onBase := &SignatureFile{CLAVersion: "v2", Signatures: []Signature{
		{Login: "alice", ID: 1, Date: "2026-08-31", CLA: "v1"},
	}}

	if err := maySign(Principal{ID: 1, Login: "alice", Type: "User"}, people, head, onBase, "v2"); err != nil {
		t.Fatalf("maySign at a bumped version = %v, want nil", err)
	}
}

func TestMaySignRefusesABot(t *testing.T) {
	people := []Principal{{ID: 49699333, Login: "dependabot[bot]", Type: "Bot"}}
	head := &SignatureFile{CLAVersion: "v1"}
	onBase := &SignatureFile{CLAVersion: "v1"}

	err := maySign(Principal{ID: 49699333, Login: "dependabot[bot]", Type: "Bot"}, people, head, onBase, "v1")
	if !errors.Is(err, errBotCommenter) {
		t.Fatalf("maySign for a bot = %v, want errBotCommenter", err)
	}
}

// signable builds a pull request opened by alice with one commit of their own,
// nothing signed anywhere. The base branch and the head agree, which is the state
// a first-time contributor's pull request is actually in.
func signable() *fakeGitHub {
	gh := &fakeGitHub{
		files: map[string]*SignatureFile{
			"main":     {CLAVersion: "v1", Signatures: []Signature{}},
			"deadbeef": {CLAVersion: "v1", Signatures: []Signature{}},
		},
		fileSHA: "abc123",
		pr:      PullRequest{Number: 107, State: "open", Commits: 1, User: Principal{ID: 1, Login: "alice", Type: "User"}},
		commits: []Commit{{SHA: "c1", Author: &Principal{ID: 1, Login: "alice", Type: "User"}}},
	}
	gh.pr.Head.SHA = "deadbeef"
	gh.pr.Base.Ref = "main"
	return gh
}

func newSigner(gh githubAPI, commenter Principal) *signer {
	return &signer{
		cfg: signConfig{repo: "pug-sh/pug", pr: 107, token: "t", commenter: commenter},
		gh:  gh,
		now: func() time.Time { return fixedNow },
	}
}

var alice = Principal{ID: 1, Login: "alice", Type: "User"}

func TestSignAppendsTheCommenterAndCommitsToTheBaseBranch(t *testing.T) {
	gh := signable()
	if err := newSigner(gh, alice).sign(t.Context()); err != nil {
		t.Fatalf("sign: %v", err)
	}

	// The base branch, never the head: the token cannot push to a fork, which is
	// the case the gate exists for.
	if gh.putBranch != "main" {
		t.Errorf("committed to %q, want main", gh.putBranch)
	}
	if gh.putFile == nil || len(gh.putFile.Signatures) != 1 {
		t.Fatalf("wrote %+v, want one entry", gh.putFile)
	}
	got := gh.putFile.Signatures[0]
	if got.ID != 1 || got.Login != "alice" || got.CLA != "v1" {
		t.Errorf("entry = %+v, want alice at v1", got)
	}
	if got.Date != "2026-08-30" {
		t.Errorf("date = %q, want the run's UTC date", got.Date)
	}
	if gh.rerunID != 991 {
		t.Errorf("rerun id = %d, want the CLA run re-run", gh.rerunID)
	}
}

// A conflict means another signature landed between the read and the write, so
// the signer re-reads and appends to the file as it now stands.
func TestSignRetriesOnceOnAConflict(t *testing.T) {
	gh := signable()
	gh.putConflicts = 1
	if err := newSigner(gh, alice).sign(t.Context()); err != nil {
		t.Fatalf("sign after one conflict: %v", err)
	}
	if gh.putAttempts != 2 {
		t.Errorf("put attempts = %d, want 2", gh.putAttempts)
	}
}

// Two in a row is not congestion, it is a bug, and spinning would hold the runner
// while making it worse.
func TestSignGivesUpAfterASecondConflict(t *testing.T) {
	gh := signable()
	gh.putConflicts = 2
	if err := newSigner(gh, alice).sign(t.Context()); err == nil {
		t.Fatal("sign returned nil after repeated conflicts")
	}
	if gh.putAttempts != 2 {
		t.Errorf("put attempts = %d, want 2", gh.putAttempts)
	}
}

func TestSignRefusesAStrangerAndWritesNothing(t *testing.T) {
	gh := signable()
	err := newSigner(gh, Principal{ID: 999, Login: "carol", Type: "User"}).sign(t.Context())
	if !errors.Is(err, errNotAPrincipal) {
		t.Fatalf("sign for a stranger = %v, want errNotAPrincipal", err)
	}
	if gh.putAttempts != 0 {
		t.Errorf("wrote despite refusing: %d attempts", gh.putAttempts)
	}
	if len(gh.posted) != 1 || !strings.Contains(gh.posted[0].Body, "@carol") {
		t.Errorf("no reply naming the commenter: %+v", gh.posted)
	}
}

// A double /sign must not paint the pull request red: the signature is there, so
// the job is green and the reply just says so.
func TestSignIsGreenAndSilentWhenAlreadySigned(t *testing.T) {
	gh := signable()
	gh.files["main"] = &SignatureFile{CLAVersion: "v1", Signatures: []Signature{
		{Login: "alice", ID: 1, Date: "2026-08-30", CLA: "v1"},
	}}
	if err := newSigner(gh, alice).sign(t.Context()); err != nil {
		t.Fatalf("sign when already signed = %v, want nil", err)
	}
	if gh.putAttempts != 0 {
		t.Errorf("wrote a duplicate: %d attempts", gh.putAttempts)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("posted %d replies, want 1", len(gh.posted))
	}
}

// A truncated commit list would drop principals and refuse someone who really is
// one, so it is a fault rather than a refusal.
func TestSignRefusesToActOnATruncatedCommitList(t *testing.T) {
	gh := signable()
	gh.pr.Commits = 3
	err := newSigner(gh, alice).sign(t.Context())
	if err == nil || !strings.Contains(err.Error(), "commits") {
		t.Fatalf("sign on a truncated list = %v, want a commit-count error", err)
	}
	if gh.putAttempts != 0 {
		t.Errorf("wrote on an untrusted principal list: %d attempts", gh.putAttempts)
	}
}

func TestRunSignRefusesAnEmptyConfiguration(t *testing.T) {
	t.Setenv("COMMENT_BODY", signCommand)
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("PR_NUMBER", "")
	if err := runSign(t.Context()); err == nil {
		t.Fatal("runSign accepted an empty configuration")
	}
}

// Actions expressions cannot trim, so the workflow's `if:` is only a prefilter and
// the exact match happens here.
func TestRunSignIgnoresACommentThatMerelyMentionsTheCommand(t *testing.T) {
	t.Setenv("COMMENT_BODY", "I'll /sign this later, promise")
	if err := runSign(t.Context()); err != nil {
		t.Fatalf("runSign on an unrelated comment = %v, want a quiet nil", err)
	}
}

func TestRunSignToleratesSurroundingWhitespace(t *testing.T) {
	t.Setenv("COMMENT_BODY", "  /sign\r\n")
	t.Setenv("GITHUB_REPOSITORY", "")
	// Reaching the configuration error proves the body was accepted as the command.
	if err := runSign(t.Context()); err == nil {
		t.Fatal("runSign treated a padded /sign as a non-command")
	}
}
