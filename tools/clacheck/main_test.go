package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	lookupErr error
	base      string // ref check should read the base file at; defaults to "base"

	posted   []Comment
	edits    int
	listErr  error
	writeErr error

	labelled    []string
	labelWrites int
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

func (f *fakeGitHub) comments(context.Context, int) ([]Comment, error) {
	return f.posted, f.listErr
}

func (f *fakeGitHub) createComment(_ context.Context, _ int, body string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.posted = append(f.posted, Comment{ID: int64(len(f.posted) + 1), Body: body})
	return nil
}

func (f *fakeGitHub) updateComment(_ context.Context, id int64, body string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	for i := range f.posted {
		if f.posted[i].ID == id {
			f.posted[i].Body = body
			f.edits++
			return nil
		}
	}
	return errNotFound
}

func (f *fakeGitHub) labels(context.Context, int) ([]Label, error) {
	out := make([]Label, len(f.labelled))
	for i, n := range f.labelled {
		out[i] = Label{Name: n}
	}
	return out, f.listErr
}

func (f *fakeGitHub) addLabel(_ context.Context, _ int, name string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.labelWrites++
	f.labelled = append(f.labelled, name)
	return nil
}

func (f *fakeGitHub) removeLabel(_ context.Context, _ int, name string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.labelWrites++
	f.labelled = slices.DeleteFunc(f.labelled, func(n string) bool { return n == name })
	return nil
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
	// alice opened it, so hers is the entry offered; bob authored the commit and
	// is named, but only he can sign for himself.
	for _, want := range []string{"alice", "bob", `"id":    1`, "opened themselves"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("want %q in the report, got %q", want, out.String())
		}
	}
	if strings.Contains(out.String(), `"id":    2`) {
		t.Fatalf("bob must not be handed an entry this pull request cannot add, got %q", out.String())
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
	if !strings.Contains(string(body), "## CLA signature required") {
		t.Fatalf("want the summary heading, got %q", body)
	}
}

// A rate limit is not evidence that carol has no account, so it must not reach
// her as "unidentified" — that sends the contributor off to fix an address that
// was fine. It is the checker's fault and is raised as one.
func TestResolveCoauthorsRaisesALookupFailure(t *testing.T) {
	commits := []Commit{commit("a1", user("alice", 1), user("alice", 1),
		"feat: thing\n\nCo-authored-by: Carol <carol@users.noreply.github.com>\n")}

	c := newChecker(&fakeGitHub{lookupErr: errors.New("403 rate limit exceeded")}, &bytes.Buffer{})
	_, unknown, err := c.resolveCoauthors(t.Context(), commits)
	if err == nil || len(unknown) != 0 {
		t.Fatalf("want the failure raised, got unknown=%v err=%v", unknown, err)
	}
}

// A plaintext address is not resolved at all: user search sees only emails made
// public on a profile, so it answers for a minority and costs the strictest rate
// limit we touch. It reaches the contributor through the report, not as a fault.
func TestResolveCoauthorsReportsAPlaintextAddress(t *testing.T) {
	commits := []Commit{commit("a1", user("alice", 1), user("alice", 1),
		"feat: thing\n\nCo-authored-by: Carol <carol@example.com>\n")}

	c := newChecker(&fakeGitHub{}, &bytes.Buffer{})
	found, unknown, err := c.resolveCoauthors(t.Context(), commits)
	if err != nil || len(found) != 0 || len(unknown) != 1 || unknown[0] != "carol@example.com" {
		t.Fatalf("want the address reported, got found=%v unknown=%v err=%v", found, unknown, err)
	}
}

// An assistant holds no copyright, so its trailer is skipped rather than blocking
// — otherwise every contributor using one rewrites their branch to merge. The
// pairing matters: an address that might belong to a person must still stop the
// gate, or the exemption is a hole rather than a rule.
func TestAnAssistantTrailerIsSkippedButAPersonsIsNot(t *testing.T) {
	msg := "feat: thing\n\nCo-authored-by: Claude <noreply@anthropic.com>\n"
	c := newChecker(&fakeGitHub{}, &bytes.Buffer{})
	found, unknown, err := c.resolveCoauthors(t.Context(),
		[]Commit{commit("a1", user("alice", 1), user("alice", 1), msg)})
	if err != nil || len(found) != 0 || len(unknown) != 0 {
		t.Fatalf("an assistant licenses nothing and must not block, got found=%v unknown=%v err=%v", found, unknown, err)
	}

	person := "feat: thing\n\nCo-authored-by: Carol <carol@anthropic.com>\n"
	_, unknown, err = c.resolveCoauthors(t.Context(),
		[]Commit{commit("a1", user("alice", 1), user("alice", 1), person)})
	if err != nil || len(unknown) != 1 {
		t.Fatalf("a colleague at the same domain still holds copyright, got unknown=%v err=%v", unknown, err)
	}
}

// The gate still blocks — a trailer names a copyright holder either way — but the
// contributor must meet the ordinary report, not an error that hides it.
func TestUnidentifiedCoauthorBlocksThroughTheReport(t *testing.T) {
	var out bytes.Buffer
	gh := &fakeGitHub{
		files: map[string]*SignatureFile{"head": file(sig("alice", 1)), "base": file(sig("alice", 1))},
		commits: []Commit{commit("a1", user("alice", 1), user("alice", 1),
			"feat\n\nCo-authored-by: Carol <carol@example.com>\n")},
	}
	c := newChecker(gh, &out)

	if err := c.check(t.Context()); !errors.Is(err, errUnsigned) {
		t.Fatalf("want errUnsigned, got %v", err)
	}
	for _, want := range []string{"::error::", "carol@example.com", "drop the trailer"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("want %q in the report, got %q", want, out.String())
		}
	}
	if len(gh.posted) != 1 || !strings.Contains(gh.posted[0].Body, "carol@example.com") {
		t.Errorf("want the address in the comment too, got %+v", gh.posted)
	}
}

func TestResolveCoauthorsFallsBackToTheAPIForTheIDLessNoreplyForm(t *testing.T) {
	commits := []Commit{commit("a1", user("alice", 1), user("alice", 1),
		"feat: thing\n\nCo-authored-by: Bob <bob@users.noreply.github.com>\n")}

	c := newChecker(&fakeGitHub{byLogin: map[string]Principal{"bob": {ID: 99, Login: "bob", Type: "User"}}}, &bytes.Buffer{})
	found, unknown, err := c.resolveCoauthors(t.Context(), commits)
	if err != nil || len(unknown) != 0 || len(found) != 1 || found[0].ID != 99 {
		t.Fatalf("want bob resolved to id 99, got found=%v unknown=%v err=%v", found, unknown, err)
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
		// "0" parses, so only the explicit guard catches it — and an id of zero is
		// dropped by unsigned(), which would empty the check rather than fail it.
		{"zero opener", map[string]string{"PR_USER_ID": "0"}, "PR_USER_ID is zero"},
		// appendOnly matches the entry's login against the opener's, so an empty
		// one refuses every signature the report hands out.
		{"no opener login", map[string]string{"PR_USER_LOGIN": ""}, "PR_USER_LOGIN is empty"},
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
			"head": file(sig("mallory", 2), Signature{Login: "victim", ID: 42, Date: "2026-08-30", CLA: "v1"}),
			"base": file(sig("mallory", 2)),
		},
		commits: []Commit{commit("a1", user("mallory", 2), user("mallory", 2),
			"feat\n\nCo-authored-by: V <42+bob@users.noreply.github.com>\n")},
		byLogin: map[string]Principal{"bob": {ID: 99, Login: "bob", Type: "User"}},
	}, &out)
	c.cfg.opener = Principal{ID: 2, Login: "mallory", Type: "User"}

	err := c.check(t.Context())
	if !errors.Is(err, errUnsigned) || !strings.Contains(out.String(), "you may only sign for yourself") {
		t.Fatalf("want the stranger signature rejected, got %v (out=%q)", err, out.String())
	}
}

// The trailer above named a login that resolves to nobody. Naming a real one is
// the sharper attack: the id then comes back from the API and is genuinely the
// victim's, so only refusing every signature but the opener's stops it.
func TestNamingARealCoauthorStillCannotSignForThem(t *testing.T) {
	var out bytes.Buffer
	c := newChecker(&fakeGitHub{
		files: map[string]*SignatureFile{
			"head": file(sig("mallory", 2), sig("victim", 777)),
			"base": file(sig("mallory", 2)),
		},
		commits: []Commit{commit("a1", user("mallory", 2), user("mallory", 2),
			"feat\n\nCo-authored-by: Victim <victim@users.noreply.github.com>\n")},
		byLogin: map[string]Principal{"victim": {ID: 777, Login: "victim", Type: "User"}},
	}, &out)
	c.cfg.opener = Principal{ID: 2, Login: "mallory", Type: "User"}

	err := c.check(t.Context())
	if !errors.Is(err, errUnsigned) || !strings.Contains(out.String(), "who did not open it") {
		t.Fatalf("want the stranger signature rejected, got %v (out=%q)", err, out.String())
	}
}

// `git commit --author=` is free to set, so GitHub attributes the commit to
// whoever the address belongs to. That made the commit author a second way to
// name a victim as a principal and then sign for them.
func TestForgedCommitAuthorCannotSignForAStranger(t *testing.T) {
	var out bytes.Buffer
	c := newChecker(&fakeGitHub{
		files: map[string]*SignatureFile{
			"head": file(sig("mallory", 2), sig("victim", 777)),
			"base": file(sig("mallory", 2)),
		},
		commits: []Commit{commit("a1", user("victim", 777), user("mallory", 2), "feat")},
	}, &out)
	c.cfg.opener = Principal{ID: 2, Login: "mallory", Type: "User"}

	err := c.check(t.Context())
	if !errors.Is(err, errUnsigned) || !strings.Contains(out.String(), "who did not open it") {
		t.Fatalf("want the stranger signature rejected, got %v (out=%q)", err, out.String())
	}
}

// The noreply form resolving to nobody must block, not be silently skipped:
// dropping it let the gate pass while never checking that person at all.
func TestUnresolvableCoauthorIsReported(t *testing.T) {
	var out bytes.Buffer
	c := newChecker(&fakeGitHub{
		files: map[string]*SignatureFile{"head": file(sig("mallory", 2)), "base": file(sig("mallory", 2))},
		commits: []Commit{commit("a1", user("mallory", 2), user("mallory", 2),
			"feat\n\nCo-authored-by: Real Person <0+realperson@users.noreply.github.com>\n")},
	}, &out)
	c.cfg.opener = Principal{ID: 2, Login: "mallory", Type: "User"}

	if err := c.check(t.Context()); !errors.Is(err, errUnsigned) {
		t.Fatalf("want the co-author to block the gate, got %v (out=%q)", err, out.String())
	}
	if !strings.Contains(out.String(), "0+realperson@users.noreply.github.com") {
		t.Fatalf("want the address named in the report, got %q", out.String())
	}
}

// dependabot and friends author no human copyright, so there is nothing to sign.
// Failing these shut every dependency update out of the repository.
func TestAllBotPullRequestPasses(t *testing.T) {
	var out bytes.Buffer
	bot := &Principal{ID: 49699333, Login: "dependabot[bot]", Type: "Bot"}
	gh := &fakeGitHub{
		files:   map[string]*SignatureFile{"head": file(), "base": file()},
		commits: []Commit{commit("a1", bot, bot, "chore(deps): bump x")},
		posted:  []Comment{{ID: 9, Body: commentMarker + "\n## Signature required"}},
	}
	c := newChecker(gh, &out)
	c.cfg.opener = *bot

	if err := c.check(t.Context()); err != nil {
		t.Fatalf("want a pass, got %v", err)
	}
	if !strings.Contains(out.String(), "nothing to sign") {
		t.Fatalf("want the bot-only line, got %q", out.String())
	}
	// Rebasing a human commit away leaves nobody to demand a signature from.
	if gh.edits != 1 || strings.Contains(gh.posted[0].Body, "Signature required") {
		t.Fatalf("want the standing demand resolved, got %q", gh.posted[0].Body)
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
		{Login: "alice", ID: 1, Date: "2026-01-01", CLA: "v1"},
	}}
	err := appendOnly(file(sig("alice", 1)), head, Principal{ID: 1, Login: "alice", Type: "User"}, "v1")
	if err == nil || !strings.Contains(err.Error(), "cla_version is") {
		t.Fatalf("want the version change rejected, got %v", err)
	}
}

// A login is contributor-controlled text out of signatures/cla.json. Emitted raw,
// a newline in one starts a second workflow command on the line below it.
func TestAnnotationEscapingKeepsAnErrorToOneCommand(t *testing.T) {
	f := file(Signature{Login: "a\n::error::injected", ID: 1, Date: "bad", CLA: "v1"})
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

// An unsigned run must reach the contributor somewhere other than a job log, and
// must name them in a form GitHub notifies on.
func TestCheckCommentsMentioningTheUnsignedContributor(t *testing.T) {
	gh := &fakeGitHub{
		files:   map[string]*SignatureFile{"head": file(), "base": file()},
		commits: []Commit{commit("a1", user("alice", 1), user("alice", 1), "x")},
	}
	c := newChecker(gh, &bytes.Buffer{})

	if err := c.check(t.Context()); !errors.Is(err, errUnsigned) {
		t.Fatalf("want errUnsigned, got %v", err)
	}
	if len(gh.posted) != 1 {
		t.Fatalf("want one comment, got %d", len(gh.posted))
	}
	for _, want := range []string{commentMarker, "@alice", `"id":    1`} {
		if !strings.Contains(gh.posted[0].Body, want) {
			t.Fatalf("want %q in the comment, got %q", want, gh.posted[0].Body)
		}
	}
}

// A contributor pushing repeatedly must be notified once. The marked comment is
// edited in place; a second one would be a new notification every push.
func TestCheckEditsItsOwnCommentInsteadOfPostingAnother(t *testing.T) {
	gh := &fakeGitHub{
		files:   map[string]*SignatureFile{"head": file(), "base": file()},
		commits: []Commit{commit("a1", user("bob", 2), user("bob", 2), "x")},
		posted:  []Comment{{ID: 5, Body: "someone else's review"}, {ID: 9, Body: commentMarker + "\nstale"}},
	}
	c := newChecker(gh, &bytes.Buffer{})

	if err := c.check(t.Context()); !errors.Is(err, errUnsigned) {
		t.Fatalf("want errUnsigned, got %v", err)
	}
	if len(gh.posted) != 2 || gh.edits != 1 {
		t.Fatalf("want the marked comment edited and none added, got %d comments and %d edits", len(gh.posted), gh.edits)
	}
	if !strings.Contains(gh.posted[1].Body, "bob") || strings.Contains(gh.posted[1].Body, "stale") {
		t.Fatalf("want the marked comment replaced, got %q", gh.posted[1].Body)
	}
}

// An unchanged report must not be rewritten: an edit bumps the comment's
// timestamp and reads as news to everyone watching the pull request.
func TestCheckLeavesAnUnchangedCommentAlone(t *testing.T) {
	gh := &fakeGitHub{
		files:   map[string]*SignatureFile{"head": file(), "base": file()},
		commits: []Commit{commit("a1", user("bob", 2), user("bob", 2), "x")},
	}
	c := newChecker(gh, &bytes.Buffer{})
	if err := c.check(t.Context()); !errors.Is(err, errUnsigned) {
		t.Fatalf("want errUnsigned, got %v", err)
	}
	if err := c.check(t.Context()); !errors.Is(err, errUnsigned) {
		t.Fatalf("want errUnsigned on the re-run, got %v", err)
	}
	if len(gh.posted) != 1 || gh.edits != 0 {
		t.Fatalf("want the same comment untouched, got %d comments and %d edits", len(gh.posted), gh.edits)
	}
}

// Once signed, the demand has been met: the comment is replaced rather than left
// standing on a merged pull request. A pull request that was never asked gets no
// comment at all.
func TestCheckResolvesItsCommentOnceSigned(t *testing.T) {
	gh := &fakeGitHub{
		files:   map[string]*SignatureFile{"head": file(sig("alice", 1)), "base": file(sig("alice", 1))},
		commits: []Commit{commit("a1", user("alice", 1), user("alice", 1), "x")},
		posted:  []Comment{{ID: 9, Body: commentMarker + "\n## Signature required"}},
	}
	c := newChecker(gh, &bytes.Buffer{})

	if err := c.check(t.Context()); err != nil {
		t.Fatalf("want a pass, got %v", err)
	}
	if len(gh.posted) != 1 || !strings.Contains(gh.posted[0].Body, "CLA v1 signed") {
		t.Fatalf("want the request replaced by the signed note, got %+v", gh.posted)
	}

	quiet := &fakeGitHub{
		files:   map[string]*SignatureFile{"head": file(sig("alice", 1)), "base": file(sig("alice", 1))},
		commits: []Commit{commit("a1", user("alice", 1), user("alice", 1), "x")},
	}
	if err := newChecker(quiet, &bytes.Buffer{}).check(t.Context()); err != nil {
		t.Fatalf("want a pass, got %v", err)
	}
	if len(quiet.posted) != 0 {
		t.Fatalf("a signed pull request must not be commented on, got %+v", quiet.posted)
	}
}

// The comment is best-effort: a repository whose token cannot comment must still
// fail on the signature, not on the notification.
func TestCheckKeepsItsVerdictWhenCommentingFails(t *testing.T) {
	gh := &fakeGitHub{
		files:    map[string]*SignatureFile{"head": file(), "base": file()},
		commits:  []Commit{commit("a1", user("bob", 2), user("bob", 2), "x")},
		writeErr: errors.New("403 resource not accessible by integration"),
	}
	if err := newChecker(gh, &bytes.Buffer{}).check(t.Context()); !errors.Is(err, errUnsigned) {
		t.Fatalf("want errUnsigned, got %v", err)
	}

	signed := &fakeGitHub{
		files:   map[string]*SignatureFile{"head": file(sig("alice", 1)), "base": file(sig("alice", 1))},
		commits: []Commit{commit("a1", user("alice", 1), user("alice", 1), "x")},
		posted:  []Comment{{ID: 9, Body: commentMarker + "\n## Signature required"}},
		listErr: errors.New("403 resource not accessible by integration"),
	}
	if err := newChecker(signed, &bytes.Buffer{}).check(t.Context()); err != nil {
		t.Fatalf("a failed comment must not fail a signed pull request, got %v", err)
	}

	// A refused edit must annotate: the run exits 0, so its log is the one
	// nobody opens and the gate would go quiet unnoticed.
	var out bytes.Buffer
	refused := &fakeGitHub{
		files:    map[string]*SignatureFile{"head": file(sig("alice", 1)), "base": file(sig("alice", 1))},
		commits:  []Commit{commit("a1", user("alice", 1), user("alice", 1), "x")},
		posted:   []Comment{{ID: 9, Body: commentMarker + "\n## Signature required"}},
		writeErr: errors.New("403 resource not accessible by integration"),
	}
	if err := newChecker(refused, &out).check(t.Context()); err != nil {
		t.Fatalf("a failed edit must not fail a signed pull request, got %v", err)
	}
	if !strings.Contains(out.String(), "::warning::") {
		t.Fatalf("want a warning annotation, got %q", out.String())
	}
}

// A comment that merely quotes the gate is not the gate's: GitHub's quote-reply
// copies the marker in, and editing it would silence the gate for good.
func TestCheckIgnoresAQuotedMarker(t *testing.T) {
	gh := &fakeGitHub{
		files:   map[string]*SignatureFile{"head": file(), "base": file()},
		commits: []Commit{commit("a1", user("alice", 1), user("alice", 1), "x")},
		posted:  []Comment{{ID: 5, Body: "> " + commentMarker + "\n> ## Signature required\n\nwhy?"}},
	}
	if err := newChecker(gh, &bytes.Buffer{}).check(t.Context()); !errors.Is(err, errUnsigned) {
		t.Fatalf("want errUnsigned, got %v", err)
	}
	if len(gh.posted) != 2 || gh.edits != 0 {
		t.Fatalf("want its own comment posted and the quote untouched, got %d comments and %d edits", len(gh.posted), gh.edits)
	}
}

// A gate that fell over must not leave "signed" standing on a red check, nor
// repeat advice the contributor has just followed and failed on.
func TestCheckReplacesItsCommentWhenTheGateCannotFinish(t *testing.T) {
	// No file at the head sha: the gate cannot read what it is meant to judge.
	forged := func() *fakeGitHub {
		return &fakeGitHub{
			files:   map[string]*SignatureFile{"base": file()},
			commits: []Commit{commit("a1", user("alice", 1), user("alice", 1), "x")},
		}
	}

	gh := forged()
	gh.posted = []Comment{{ID: 9, Body: signedComment("v1")}}
	if err := newChecker(gh, &bytes.Buffer{}).check(t.Context()); err == nil || errors.Is(err, errUnsigned) {
		t.Fatalf("want a checker fault, got %v", err)
	}
	if gh.edits != 1 || strings.Contains(gh.posted[0].Body, "signed") {
		t.Fatalf("want the signed note replaced, got %q after %d edits", gh.posted[0].Body, gh.edits)
	}

	// Nothing standing means nothing to correct; a bare failure is not worth a comment.
	quiet := forged()
	if err := newChecker(quiet, &bytes.Buffer{}).check(t.Context()); err == nil {
		t.Fatal("want a checker fault")
	}
	if len(quiet.posted) != 0 {
		t.Fatalf("want no comment, got %+v", quiet.posted)
	}
}

// The label is what makes CLA status visible in the pull request list, and the
// two are mutually exclusive: a signed pull request still carrying "not signed"
// is worse than no label at all.
func TestCheckLabelsEachOutcome(t *testing.T) {
	unsignedPR := func() *fakeGitHub {
		return &fakeGitHub{
			files:    map[string]*SignatureFile{"head": file(), "base": file()},
			commits:  []Commit{commit("a1", user("alice", 1), user("alice", 1), "x")},
			labelled: []string{"enhancement", labelSigned},
		}
	}
	gh := unsignedPR()
	if err := newChecker(gh, &bytes.Buffer{}).check(t.Context()); !errors.Is(err, errUnsigned) {
		t.Fatalf("want errUnsigned, got %v", err)
	}
	if !slices.Equal(gh.labelled, []string{"enhancement", labelUnsigned}) {
		t.Fatalf("want the signed label swapped out and enhancement kept, got %v", gh.labelled)
	}

	signed := &fakeGitHub{
		files:    map[string]*SignatureFile{"head": file(sig("alice", 1)), "base": file(sig("alice", 1))},
		commits:  []Commit{commit("a1", user("alice", 1), user("alice", 1), "x")},
		labelled: []string{labelUnsigned},
	}
	if err := newChecker(signed, &bytes.Buffer{}).check(t.Context()); err != nil {
		t.Fatalf("want a pass, got %v", err)
	}
	if !slices.Equal(signed.labelled, []string{labelSigned}) {
		t.Fatalf("want only the signed label, got %v", signed.labelled)
	}

	// A gate that could not finish knows nothing, but must not leave "signed"
	// standing on a red check.
	broken := &fakeGitHub{
		files:    map[string]*SignatureFile{"base": file()},
		commits:  []Commit{commit("a1", user("alice", 1), user("alice", 1), "x")},
		labelled: []string{labelSigned},
	}
	if err := newChecker(broken, &bytes.Buffer{}).check(t.Context()); err == nil || errors.Is(err, errUnsigned) {
		t.Fatalf("want a checker fault, got %v", err)
	}
	if len(broken.labelled) != 0 {
		t.Fatalf("want the signed label dropped, got %v", broken.labelled)
	}
}

// A label event notifies everyone watching the pull request, so a run that
// changes nothing must write nothing.
func TestCheckDoesNotRewriteACorrectLabel(t *testing.T) {
	gh := &fakeGitHub{
		files:    map[string]*SignatureFile{"head": file(), "base": file()},
		commits:  []Commit{commit("a1", user("alice", 1), user("alice", 1), "x")},
		labelled: []string{labelUnsigned},
	}
	if err := newChecker(gh, &bytes.Buffer{}).check(t.Context()); !errors.Is(err, errUnsigned) {
		t.Fatalf("want errUnsigned, got %v", err)
	}
	if gh.labelWrites != 0 {
		t.Fatalf("want no label write, got %d", gh.labelWrites)
	}
}

// The signature file is absent at the merge base only if the read failed, now
// that it exists on the base branch. Treating that as an empty history disarmed
// both append-only guards, so a pull request could name its own cla_version.
func TestAMissingBaseFileIsAFailureNotAnEmptyHistory(t *testing.T) {
	var out bytes.Buffer
	c := newChecker(&fakeGitHub{
		base: "mergebase", // no file there
		files: map[string]*SignatureFile{
			"base": file(),
			"head": {CLAVersion: "vBOGUS", Signatures: []Signature{
				{Login: "mallory", ID: 7, Date: "2026-08-30", CLA: "vBOGUS"}}},
		},
		commits: []Commit{commit("a1", user("mallory", 7), user("mallory", 7), "feat")},
	}, &out)
	c.cfg.opener = Principal{ID: 7, Login: "mallory", Type: "User"}

	err := c.check(t.Context())
	if err == nil || errors.Is(err, errUnsigned) {
		t.Fatalf("want a read failure, not a verdict, got %v (out=%q)", err, out.String())
	}
	if strings.Contains(out.String(), "verified") {
		t.Fatalf("a self-declared cla_version must never read as verified, got %q", out.String())
	}
}

// A rejected edit is the contributor's to fix, so it takes the unsigned label and
// a comment rather than reading as a gate that fell over.
func TestARejectedEditIsReportedLikeAnUnsignedCLA(t *testing.T) {
	var out bytes.Buffer
	gh := &fakeGitHub{
		files: map[string]*SignatureFile{
			"head": file(sig("mallory", 2), sig("victim", 777)),
			"base": file(sig("mallory", 2)),
		},
		commits:  []Commit{commit("a1", user("mallory", 2), user("mallory", 2), "feat")},
		labelled: []string{labelSigned},
	}
	c := newChecker(gh, &out)
	c.cfg.opener = Principal{ID: 2, Login: "mallory", Type: "User"}

	if err := c.check(t.Context()); !errors.Is(err, errUnsigned) {
		t.Fatalf("want errUnsigned, got %v", err)
	}
	if !slices.Equal(gh.labelled, []string{labelUnsigned}) {
		t.Fatalf("want the not-signed label, got %v", gh.labelled)
	}
	if len(gh.posted) != 1 || !strings.Contains(gh.posted[0].Body, "was rejected") {
		t.Fatalf("want a comment naming the rejection, got %v", gh.posted)
	}
}

// An assistant trailer names no copyright holder, so a pull request carrying one
// and nothing else must go green without a history rewrite.
func TestAnAssistantTrailerAloneDoesNotBlockTheGate(t *testing.T) {
	for _, addr := range []string{"noreply@anthropic.com", "NoReply@Anthropic.COM"} {
		var out bytes.Buffer
		c := newChecker(&fakeGitHub{
			files: map[string]*SignatureFile{"head": file(sig("alice", 1)), "base": file(sig("alice", 1))},
			commits: []Commit{commit("a1", user("alice", 1), user("alice", 1),
				"feat\n\nCo-authored-by: Claude <"+addr+">\n")},
		}, &out)
		if err := c.check(t.Context()); err != nil {
			t.Fatalf("%s must not block: %v (out=%q)", addr, err, out.String())
		}
	}
}
