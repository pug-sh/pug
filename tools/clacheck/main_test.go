package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGitHub stands in for the API so the gate's decisions can be exercised
// without a network or a token.
type fakeGitHub struct {
	files     map[string]*SignatureFile
	commits   []Commit
	byLogin   map[string]Principal
	byEmail   map[string]Principal
	lookupErr error
	base      string // ref check should read the base file at; defaults to "base"
}

func (f *fakeGitHub) signatureFile(_ context.Context, ref string) (*SignatureFile, error) {
	if sf, ok := f.files[ref]; ok {
		return sf, nil
	}
	return nil, errNotFound
}

func (f *fakeGitHub) pullCommits(context.Context, int) ([]Commit, error) { return f.commits, nil }

func (f *fakeGitHub) mergeBase(context.Context, string, string) (string, error) {
	if f.base != "" {
		return f.base, nil
	}
	return "base", nil
}

func (f *fakeGitHub) userByLogin(_ context.Context, login string) (Principal, error) {
	if f.lookupErr != nil {
		return Principal{}, f.lookupErr
	}
	if p, ok := f.byLogin[login]; ok {
		return p, nil
	}
	return Principal{}, errNotFound
}

func (f *fakeGitHub) userByEmail(_ context.Context, email string) (Principal, error) {
	if f.lookupErr != nil {
		return Principal{}, f.lookupErr
	}
	if p, ok := f.byEmail[email]; ok {
		return p, nil
	}
	return Principal{}, errNotFound
}

var fixedNow = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

func newChecker(gh githubAPI, out *bytes.Buffer) *checker {
	return &checker{
		cfg: config{
			repo:      "pug-sh/pug",
			pr:        7,
			prCommits: 1,
			headSHA:   "head",
			baseSHA:   "base",
			baseRef:   "main",
			serverURL: "https://github.com",
			opener:    Principal{ID: 1, Login: "alice", Type: "User"},
		},
		gh:  gh,
		out: out,
		now: func() time.Time { return fixedNow },
	}
}

func TestCheckPassesWhenEveryPrincipalHasSigned(t *testing.T) {
	var out bytes.Buffer
	c := newChecker(&fakeGitHub{
		files:   map[string]*SignatureFile{"head": file(sig("alice", 1)), "base": file(sig("alice", 1))},
		commits: []Commit{commit("a1", user("alice", 1), user("alice", 1), "feat: thing")},
	}, &out)

	if err := c.check(t.Context()); err != nil {
		t.Fatalf("want a pass, got %v", err)
	}
	if !strings.Contains(out.String(), "CLA v1 verified") {
		t.Fatalf("want the verified line, got %q", out.String())
	}
}

func TestCheckReportsTheUnsignedContributor(t *testing.T) {
	var out bytes.Buffer
	c := newChecker(&fakeGitHub{
		files:   map[string]*SignatureFile{"head": file(), "base": file()},
		commits: []Commit{commit("a1", user("bob", 2), user("bob", 2), "feat: thing")},
	}, &out)

	err := c.check(t.Context())
	if !errors.Is(err, errUnsigned) {
		t.Fatalf("want errUnsigned, got %v", err)
	}
	// main relies on the report carrying its own annotation; a second one would
	// show a bare sentinel on the checks page.
	if !strings.HasPrefix(out.String(), "::error::") {
		t.Fatalf("the report must open with the annotation, got %q", out.String())
	}
	for _, want := range []string{"alice", "bob", `"id":    2`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("want %q in the report, got %q", want, out.String())
		}
	}
}

func TestCheckRefusesATruncatedCommitList(t *testing.T) {
	var out bytes.Buffer
	c := newChecker(&fakeGitHub{
		files:   map[string]*SignatureFile{"head": file(sig("alice", 1)), "base": file(sig("alice", 1))},
		commits: []Commit{commit("a1", user("alice", 1), user("alice", 1), "x")},
	}, &out)
	c.cfg.prCommits = 300

	err := c.check(t.Context())
	if err == nil || !strings.Contains(err.Error(), "listed 1 of the pull request's 300 commits") {
		t.Fatalf("want the truncation refusal, got %v", err)
	}
}

func TestCheckWritesTheJobSummary(t *testing.T) {
	var out bytes.Buffer
	c := newChecker(&fakeGitHub{
		files:   map[string]*SignatureFile{"head": file(), "base": file()},
		commits: []Commit{commit("a1", user("bob", 2), user("bob", 2), "x")},
	}, &out)
	c.cfg.summaryPath = filepath.Join(t.TempDir(), "summary.md")

	if err := c.check(t.Context()); !errors.Is(err, errUnsigned) {
		t.Fatalf("want errUnsigned, got %v", err)
	}
	body, err := os.ReadFile(c.cfg.summaryPath)
	if err != nil {
		t.Fatalf("reading the summary: %v", err)
	}
	if !strings.Contains(string(body), "## Signature required") {
		t.Fatalf("want the summary heading, got %q", body)
	}
}

// A failed lookup and a genuinely unknown account reach the contributor as the
// same message, so the failure must at least be counted as unresolved rather
// than dropping the co-author from the check entirely.
func TestResolveCoauthorsTreatsALookupFailureAsUnresolved(t *testing.T) {
	commits := []Commit{commit("a1", user("alice", 1), user("alice", 1),
		"feat: thing\n\nCo-authored-by: Carol <carol@example.com>\n")}

	c := newChecker(&fakeGitHub{lookupErr: errors.New("403 rate limit exceeded")}, &bytes.Buffer{})
	found, unresolved := c.resolveCoauthors(t.Context(), commits)
	if len(found) != 0 || len(unresolved) != 1 || unresolved[0] != "carol@example.com" {
		t.Fatalf("want carol unresolved, got found=%v unresolved=%v", found, unresolved)
	}
}

func TestResolveCoauthorsFallsBackToTheAPIForTheIDLessNoreplyForm(t *testing.T) {
	commits := []Commit{commit("a1", user("alice", 1), user("alice", 1),
		"feat: thing\n\nCo-authored-by: Bob <bob@users.noreply.github.com>\n")}

	c := newChecker(&fakeGitHub{byLogin: map[string]Principal{"bob": {ID: 99, Login: "bob", Type: "User"}}}, &bytes.Buffer{})
	found, unresolved := c.resolveCoauthors(t.Context(), commits)
	if len(unresolved) != 0 || len(found) != 1 || found[0].ID != 99 {
		t.Fatalf("want bob resolved to id 99, got found=%v unresolved=%v", found, unresolved)
	}
}

func TestLoadConfigFallsBackForTheOptionalInputs(t *testing.T) {
	// Every var is set explicitly: the checker's own tests run inside Actions,
	// where GITHUB_* is already populated.
	for k, v := range map[string]string{
		"GITHUB_REPOSITORY":   "pug-sh/pug",
		"GITHUB_SERVER_URL":   "",
		"GITHUB_STEP_SUMMARY": "",
		"GH_TOKEN":            "",
		"GITHUB_TOKEN":        "t",
		"PR_NUMBER":           "7",
		"PR_COMMITS":          "3",
		"PR_HEAD_SHA":         "head",
		"PR_BASE_SHA":         "base",
		"PR_BASE_REF":         "",
		"PR_USER_ID":          "42",
		"PR_USER_LOGIN":       "carol",
		"PR_USER_TYPE":        "",
	} {
		t.Setenv(k, v)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.baseRef != "main" || cfg.serverURL != "https://github.com" || cfg.token != "t" {
		t.Fatalf("want the fallbacks applied, got %+v", cfg)
	}
	if cfg.opener != (Principal{ID: 42, Login: "carol", Type: "User"}) {
		t.Fatalf("want the opener assembled, got %+v", cfg.opener)
	}
}

func TestLoadConfigRejectsInputsItCannotCheckWithout(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no pr number", map[string]string{"PR_NUMBER": ""}, "PR_NUMBER"},
		{"no commit total", map[string]string{"PR_COMMITS": "many"}, "PR_COMMITS"},
		{"no opener", map[string]string{"PR_USER_ID": ""}, "PR_USER_ID"},
		{"no repo", map[string]string{"GITHUB_REPOSITORY": ""}, "GITHUB_REPOSITORY is empty"},
		{"no head", map[string]string{"PR_HEAD_SHA": ""}, "PR_HEAD_SHA is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range map[string]string{
				"GITHUB_REPOSITORY": "pug-sh/pug",
				"PR_NUMBER":         "7",
				"PR_COMMITS":        "1",
				"PR_HEAD_SHA":       "head",
				"PR_BASE_SHA":       "base",
				"GH_TOKEN":          "t",
				"PR_USER_ID":        "42",
			} {
				t.Setenv(k, v)
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			_, err := loadConfig()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want an error naming %q, got %v", tt.want, err)
			}
		})
	}
}

// A Co-authored-by trailer is commit-message text. Trusting the id it encoded let
// a pull request add a signature for anyone whose id it named; the id now comes
// back from the API, so the trailer cannot authorise an entry.
func TestForgedCoauthorTrailerCannotSignForAStranger(t *testing.T) {
	var out bytes.Buffer
	c := newChecker(&fakeGitHub{
		files: map[string]*SignatureFile{
			"head": file(sig("mallory", 2), Signature{Login: "victim", ID: 42, Name: "V", Date: "2026-08-30", CLA: "v1"}),
			"base": file(sig("mallory", 2)),
		},
		commits: []Commit{commit("a1", user("mallory", 2), user("mallory", 2),
			"feat\n\nCo-authored-by: V <42+bob@users.noreply.github.com>\n")},
		byLogin: map[string]Principal{"bob": {ID: 99, Login: "bob", Type: "User"}},
	}, &out)
	c.cfg.opener = Principal{ID: 2, Login: "mallory", Type: "User"}

	err := c.check(t.Context())
	if err == nil || !strings.Contains(err.Error(), "you may only sign for yourself") {
		t.Fatalf("want the stranger signature rejected, got %v (out=%q)", err, out.String())
	}
}

// An id-less or malformed co-author must be reported, not silently skipped:
// dropping it let the gate pass while never checking that person at all.
func TestUnresolvableCoauthorIsReported(t *testing.T) {
	var out bytes.Buffer
	c := newChecker(&fakeGitHub{
		files: map[string]*SignatureFile{"head": file(sig("mallory", 2)), "base": file(sig("mallory", 2))},
		commits: []Commit{commit("a1", user("mallory", 2), user("mallory", 2),
			"feat\n\nCo-authored-by: Real Person <0+realperson@users.noreply.github.com>\n")},
	}, &out)
	c.cfg.opener = Principal{ID: 2, Login: "mallory", Type: "User"}

	err := c.check(t.Context())
	if err == nil || !strings.Contains(err.Error(), "did not resolve") {
		t.Fatalf("want the co-author reported, got %v (out=%q)", err, out.String())
	}
}

// dependabot and friends author no human copyright, so there is nothing to sign.
// Failing these shut every dependency update out of the repository.
func TestAllBotPullRequestPasses(t *testing.T) {
	var out bytes.Buffer
	bot := &Principal{ID: 49699333, Login: "dependabot[bot]", Type: "Bot"}
	c := newChecker(&fakeGitHub{
		files:   map[string]*SignatureFile{"head": file(), "base": file()},
		commits: []Commit{commit("a1", bot, bot, "chore(deps): bump x")},
	}, &out)
	c.cfg.opener = *bot

	if err := c.check(t.Context()); err != nil {
		t.Fatalf("want a pass, got %v", err)
	}
	if !strings.Contains(out.String(), "nothing to sign") {
		t.Fatalf("want the bot-only line, got %q", out.String())
	}
}

// The base file is read at the merge base, not at the base branch tip. The tip
// moves as others sign, and every stale branch would then read as deleting them.
func TestStaleBranchIsNotAccusedOfDeletingASignature(t *testing.T) {
	var out bytes.Buffer
	c := newChecker(&fakeGitHub{
		base: "mergebase",
		files: map[string]*SignatureFile{
			"mergebase": file(sig("alice", 1)),
			"base":      file(sig("alice", 1), sig("dave", 4)), // main moved on
			"head":      file(sig("alice", 1), sig("bob", 3)),
		},
		commits: []Commit{commit("a1", user("bob", 3), user("bob", 3), "feat")},
	}, &out)
	c.cfg.opener = Principal{ID: 3, Login: "bob", Type: "User"}

	if err := c.check(t.Context()); err != nil {
		t.Fatalf("a branch that predates dave's signature is not tampering: %v", err)
	}
}

// cla_version belongs to the base branch: letting a pull request move it would
// let one invalidate everyone's signature at once.
func TestPullRequestCannotChangeTheCLAVersion(t *testing.T) {
	head := &SignatureFile{CLAVersion: "v2", Signatures: []Signature{
		{Login: "alice", ID: 1, Name: "A", Date: "2026-01-01", CLA: "v1"},
	}}
	err := appendOnly(file(sig("alice", 1)), head, map[int64]bool{1: true})
	if err == nil || !strings.Contains(err.Error(), "changes cla_version") {
		t.Fatalf("want the version change rejected, got %v", err)
	}
}

// A login is contributor-controlled text out of signatures/cla.json. Emitted raw,
// a newline in one starts a second workflow command on the line below it.
func TestAnnotationEscapingKeepsAnErrorToOneCommand(t *testing.T) {
	f := file(Signature{Login: "a\n::error::injected", ID: 1, Name: "N", Date: "bad", CLA: "v1"})
	err := f.validate()
	if err == nil {
		t.Fatal("want a validation error to carry the login")
	}
	// GitHub reads one command per line, so a single line is a single command —
	// the injected "::error::" survives only as inert text after its newline goes.
	line := fmt.Sprintf("::error::%s", escapeAnnotation(err.Error()))
	if strings.ContainsAny(line, "\r\n") {
		t.Fatalf("the annotation must stay on one line, got %q", line)
	}
	if !strings.Contains(line, "%0A") {
		t.Fatalf("want the newline encoded, got %q", line)
	}
}

func TestEscapeAnnotationEncodesTheSpecialCharacters(t *testing.T) {
	got := escapeAnnotation("100% done\r\nnext")
	if got != "100%25 done%0D%0Anext" {
		t.Fatalf("got %q", got)
	}
}
