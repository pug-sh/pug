// Command gate holds a pull request until everyone with work in it has signed
// the Contributor License Agreement.
//
// Everyone whose copyright can reach the repository through the pull request —
// commit author, committer, co-author, and the person who opened it — must appear
// in tools/cla/signatures.json. The file is read both at the pull request's head,
// where a hand-written signature lands, and at the base branch tip, where a /sign
// comment lands one. A hand-written edit must be append-only and may only add the
// person who opened it, so a co-author signs by commenting /sign instead.
//
// Commits and the signature file are read over the API, so no file the pull
// request controls is read off the runner. The checker's own code and workflow are
// the base branch's copies, which is the workflow's doing, not this program's.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const runTimeout = 5 * time.Minute

// errUnsigned is the gate's ordinary failure. check has already printed the
// report, which carries its own ::error:: annotation, so main must not add
// a second one.
var errUnsigned = errors.New("cla not signed")

type config struct {
	repo        string
	pr          int
	prCommits   int
	headSHA     string
	baseSHA     string
	baseRef     string
	opener      Principal
	serverURL   string
	summaryPath string
	token       string
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func loadConfig() (config, error) {
	c := config{
		repo:        os.Getenv("GITHUB_REPOSITORY"),
		headSHA:     os.Getenv("PR_HEAD_SHA"),
		baseSHA:     os.Getenv("PR_BASE_SHA"),
		baseRef:     env("PR_BASE_REF", "main"),
		serverURL:   env("GITHUB_SERVER_URL", "https://github.com"),
		summaryPath: os.Getenv("GITHUB_STEP_SUMMARY"),
		token:       env("GH_TOKEN", os.Getenv("GITHUB_TOKEN")),
	}
	var err error
	if c.pr, err = strconv.Atoi(os.Getenv("PR_NUMBER")); err != nil {
		return c, fmt.Errorf("PR_NUMBER: %w", err)
	}
	if c.prCommits, err = strconv.Atoi(os.Getenv("PR_COMMITS")); err != nil {
		return c, fmt.Errorf("PR_COMMITS: %w", err)
	}
	openerID, err := strconv.ParseInt(os.Getenv("PR_USER_ID"), 10, 64)
	if err != nil {
		return c, fmt.Errorf("PR_USER_ID: %w", err)
	}
	c.opener = Principal{ID: openerID, Login: os.Getenv("PR_USER_LOGIN"), Type: env("PR_USER_TYPE", "User")}
	// Every one of these silently weakens the gate if it is missing rather than
	// wrong: an empty ref reads as the default branch, and an absent token
	// downgrades the run to the unauthenticated rate limit.
	switch {
	case c.repo == "":
		return c, errors.New("GITHUB_REPOSITORY is empty")
	case c.headSHA == "":
		return c, errors.New("PR_HEAD_SHA is empty")
	case c.baseSHA == "":
		return c, errors.New("PR_BASE_SHA is empty")
	case c.token == "":
		return c, errors.New("GH_TOKEN is empty")
	case c.opener.ID == 0:
		return c, errors.New("PR_USER_ID is zero")
	case c.opener.Login == "":
		return c, errors.New("PR_USER_LOGIN is empty")
	}
	return c, nil
}

// githubAPI is the GitHub surface the gate depends on, named consumer-side so
// the check can be unit-tested against a fake instead of the live API. Every
// implementation must report a missing resource as errNotFound, which
// resolveCoauthors reads as "no such account" rather than as an API failure; an
// empty run list as errNoRuns; and a write refused for a stale blob sha as
// errConflict, which is the only thing the signer's retry keys on.
type githubAPI interface {
	signatureFile(ctx context.Context, ref string) (*SignatureFile, error)
	pullCommits(ctx context.Context, pr int) ([]Commit, error)
	mergeBase(ctx context.Context, base, head string) (string, error)
	userByLogin(ctx context.Context, login string) (Principal, error)
	signatureFileMeta(ctx context.Context, ref string) (*SignatureFile, string, error)
	putSignatureFile(ctx context.Context, branch string, f *SignatureFile, sha, message string, author Principal) error
	pullRequest(ctx context.Context, pr int) (PullRequest, error)
	latestWorkflowRun(ctx context.Context, workflowFile, headSHA string) (WorkflowRun, error)
	rerunWorkflow(ctx context.Context, runID int64) error
	comments(ctx context.Context, pr int) ([]Comment, error)
	createComment(ctx context.Context, pr int, body string) error
	updateComment(ctx context.Context, id int64, body string) error
	labels(ctx context.Context, pr int) ([]Label, error)
	addLabel(ctx context.Context, pr int, name string) error
	removeLabel(ctx context.Context, pr int, name string) error
}

// checker is everything the gate talks to. run assembles it once so check itself
// reaches for no ambient state — not the API, not stdout, not the clock.
type checker struct {
	cfg config
	gh  githubAPI
	out io.Writer
	now func() time.Time
}

// subcommand picks what to run. A bare invocation stays the checker, so cla.yaml
// is untouched by the signer's arrival. Anything else unrecognised is a typo, not
// a mode: falling through to the checker would report a signature that was never
// recorded, which is worse than any error.
func subcommand(args []string) (func(context.Context) error, bool) {
	if len(args) < 2 {
		return run, true
	}
	if args[1] == "sign" {
		return runSign, true
	}
	return nil, false
}

func main() {
	setupLogging()

	do, ok := subcommand(os.Args)
	if !ok {
		fmt.Printf("::error::unknown subcommand %q; use `sign` or no argument\n", escapeAnnotation(os.Args[1]))
		os.Exit(1)
	}

	// An unsigned CLA has already been reported with its own annotation; anything
	// else is a checker fault that nothing has annotated yet.
	switch err := do(context.Background()); {
	case errors.Is(err, errUnsigned):
		os.Exit(1)
	case err != nil:
		fmt.Printf("::error::%s\n", escapeAnnotation(err.Error()))
		os.Exit(1)
	}
}

// escapeAnnotation applies GitHub's workflow-command encoding. An error can carry
// a value the pull request chose — a login out of tools/cla/signatures.json — and a raw
// newline in one would start a second command on the line below.
var annotationEscaper = strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")

func escapeAnnotation(s string) string { return annotationEscaper.Replace(s) }

func run(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	c := &checker{cfg: cfg, gh: newClient(cfg.token, cfg.repo), out: os.Stdout, now: time.Now}
	return c.check(ctx)
}

// A gate that fell over must not leave "signed — thanks!" standing on a red
// check, nor advice the contributor has just followed and failed on.
func (c *checker) check(ctx context.Context) error {
	err := c.verdict(ctx)
	if err != nil && !errors.Is(err, errUnsigned) {
		c.upsertComment(ctx, problemComment(), false)
		c.syncLabels(ctx, "", labelSigned)
	}
	return err
}

func (c *checker) verdict(ctx context.Context) error {
	slog.InfoContext(ctx, "checking signatures",
		slog.String("repo", c.cfg.repo), slog.Int("pr", c.cfg.pr), slog.String("head", c.cfg.headSHA))

	head, err := c.gh.signatureFile(ctx, c.cfg.headSHA)
	if err != nil {
		return fmt.Errorf("reading tools/cla/signatures.json at the pull request head: %w", err)
	}
	if err := head.validate(); err != nil {
		return fmt.Errorf("tools/cla/signatures.json is invalid: %w", err)
	}

	commits, err := c.gh.pullCommits(ctx, c.cfg.pr)
	if err != nil {
		return fmt.Errorf("listing commits: refusing to pass on an unverified list: %w", err)
	}
	// GitHub caps this endpoint at 250 and reports the truncation as success, so
	// the count is compared against the pull request's own total.
	if len(commits) != c.cfg.prCommits {
		return fmt.Errorf("listed %d of the pull request's %d commits, so some authors would go unchecked; GitHub caps this endpoint at 250, so squash or split a pull request that large, otherwise re-run the job",
			len(commits), c.cfg.prCommits)
	}

	people, unlinked := principals(commits, c.cfg.opener)
	if len(unlinked) > 0 {
		return fmt.Errorf("these commits have an email that is not linked to a GitHub account, so their author cannot be identified: %s\n"+
			"Add the address at %s/settings/emails, or rewrite the commits to use your @users.noreply.github.com address",
			strings.Join(unlinked, ", "), c.cfg.serverURL)
	}

	// An unidentified co-author blocks the gate like an unsigned one — a trailer
	// names a copyright holder either way — but is reported rather than raised: it
	// is the contributor's to fix, and a checker error would bury the report.
	coauthors, unknown, err := resolveCoauthors(ctx, c.gh, commits)
	if err != nil {
		return err
	}
	people = append(people, coauthors...)

	base, err := c.baseFile(ctx)
	if err != nil {
		return err
	}
	// The base branch as it stands now, not the event's pinned base.sha. A
	// workflow re-run replays the original payload, so a signature committed by a
	// /sign comment after the event fired would be invisible on exactly the run
	// that has to see it. signatureFile passes its ref to ?ref=, which takes a
	// branch name; baseRef is a base-repo branch and never the pull request's.
	inForce, err := c.gh.signatureFile(ctx, c.cfg.baseRef)
	if err != nil {
		return fmt.Errorf("reading tools/cla/signatures.json on %s: %w", c.cfg.baseRef, err)
	}
	// Validated like head: unsigned now takes a passing verdict from this file, and
	// signedAt compares only the id and the version.
	if err := inForce.validate(); err != nil {
		return fmt.Errorf("tools/cla/signatures.json on %s is invalid: %w", c.cfg.baseRef, err)
	}
	if err := appendOnly(base, head, c.cfg.opener, inForce.CLAVersion); err != nil {
		// A rejected edit is the contributor's to fix, like an unsigned CLA, so it
		// takes the same label and comment rather than reading as a broken gate.
		fmt.Fprintf(c.out, "::error::%s\n", escapeAnnotation(err.Error()))
		c.upsertComment(ctx, rejectedComment(), true)
		c.syncLabels(ctx, labelUnsigned, labelSigned)
		return errUnsigned
	}

	missing, checked := unsigned(head, inForce, people)
	if len(missing) > 0 || len(unknown) > 0 {
		report := unsignedReport(c.cfg, head, missing, unknown, c.now())
		fmt.Fprint(c.out, report.text)
		c.writeSummary(ctx, report.markdown)
		c.upsertComment(ctx, report.comment, true)
		c.syncLabels(ctx, labelUnsigned, labelSigned)
		return errUnsigned
	}
	// A pull request authored entirely by bots — dependabot and friends — has no
	// human copyright to license, so there is nothing to sign for.
	if len(checked) == 0 {
		fmt.Fprintf(c.out, "CLA %s: no human authors across %d commit(s); nothing to sign\n", head.CLAVersion, len(commits))
		c.upsertComment(ctx, signedComment(head.CLAVersion), false)
		c.syncLabels(ctx, labelSigned, labelUnsigned)
		return nil
	}

	logins := make([]string, len(checked))
	for i, p := range checked {
		logins[i] = p.Login
	}
	fmt.Fprintf(c.out, "CLA %s verified for %d principal(s) across %d commit(s): %s\n",
		head.CLAVersion, len(checked), len(commits), strings.Join(logins, ", "))
	c.upsertComment(ctx, signedComment(head.CLAVersion), false)
	c.syncLabels(ctx, labelSigned, labelUnsigned)
	return nil
}

// syncLabels reads first so a run that changes nothing writes nothing: a label
// event fires every subscriber on the pull request. Best-effort, like the comment.
func (c *checker) syncLabels(ctx context.Context, add, remove string) {
	current, err := c.gh.labels(ctx, c.cfg.pr)
	if err != nil {
		c.writeFailed(ctx, "could not list the pull request labels", err)
		return
	}
	has := func(name string) bool {
		return slices.ContainsFunc(current, func(l Label) bool { return l.Name == name })
	}
	if remove != "" && has(remove) {
		if err := c.gh.removeLabel(ctx, c.cfg.pr, remove); err != nil {
			c.writeFailed(ctx, "could not remove the stale cla label", err)
		}
	}
	if add != "" && !has(add) {
		if err := c.gh.addLabel(ctx, c.cfg.pr, add); err != nil {
			c.writeFailed(ctx, "could not add the cla label", err)
		}
	}
}

// upsertComment edits the marked comment in place, so a contributor pushing five
// times is notified once. Best-effort: failing the gate because a comment did not
// post would block a pull request that is signed.
func (c *checker) upsertComment(ctx context.Context, body string, create bool) {
	existing, err := c.gh.comments(ctx, c.cfg.pr)
	if err != nil {
		c.writeFailed(ctx, "could not list the pull request comments", err)
		return
	}
	for _, cm := range existing {
		// Prefix, not contains: quote-reply copies the marker into the quoting
		// user's comment, and the token cannot edit that one anyway.
		if !strings.HasPrefix(cm.Body, commentMarker) {
			continue
		}
		if cm.Body == body {
			return
		}
		if err := c.gh.updateComment(ctx, cm.ID, body); err != nil {
			c.writeFailed(ctx, "could not update the signature comment", err)
		}
		return
	}
	if !create {
		return
	}
	if err := c.gh.createComment(ctx, c.cfg.pr, body); err != nil {
		c.writeFailed(ctx, "could not post the signature comment", err)
	}
}

// Annotates too: the signed run that most needs this seen exits 0, and nobody
// opens a green job's log.
func (c *checker) writeFailed(ctx context.Context, msg string, err error) {
	slog.ErrorContext(ctx, msg, slog.String("repo", c.cfg.repo), slog.Int("pr", c.cfg.pr), errAttr(err))
	fmt.Fprintf(c.out, "::warning::%s\n", escapeAnnotation(msg))
}

// baseFile reads the signature file as it stands at the merge base. The event's
// base.sha is the base branch tip, which moves as other pull requests merge, so
// comparing against it reads a signature added meanwhile as one this pull request
// deleted.
func (c *checker) baseFile(ctx context.Context) (*SignatureFile, error) {
	mergeBase, err := c.gh.mergeBase(ctx, c.cfg.baseSHA, c.cfg.headSHA)
	if err != nil {
		return nil, fmt.Errorf("finding where this pull request left %s: %w", c.cfg.baseRef, err)
	}
	base, err := c.gh.signatureFile(ctx, mergeBase)
	if err != nil {
		return nil, fmt.Errorf("reading tools/cla/signatures.json at %s: %w", mergeBase, err)
	}
	return base, nil
}

// resolveCoauthors turns Co-authored-by trailers into principals. It takes the
// API rather than hanging off the checker: the signer needs exactly this list to
// decide who may sign, and two implementations of "who is a principal" — one
// deciding who must sign and one deciding who may — is the single disagreement
// this system cannot survive. A trailer is
// commit-message text, so nothing in it is taken on trust — the address only
// chooses which login is looked up, and the id always comes back from the API.
//
// Only the noreply form is resolved. Any other address would have to go through
// user search, which sees only emails public on a profile, so it answers for a
// minority of people and spends the strictest rate limit we touch to do it; a
// commit's own author needs none of this, because GitHub resolves that one
// server-side against emails we cannot see.
//
// An unresolved address comes back as unknown for the report; an API that failed
// to answer is an error instead, since "we could not reach GitHub" must not reach
// the contributor as "your co-author has not signed". A known assistant is
// neither: it names no copyright holder, so it is skipped rather than reported.
func resolveCoauthors(ctx context.Context, gh githubAPI, commits []Commit) (found []Principal, unknown []string, err error) {
	for _, email := range coauthorEmails(commits) {
		if isAssistant(email) {
			continue
		}
		login := noreplyLogin(email)
		if login == "" {
			unknown = append(unknown, email)
			continue
		}
		p, err := gh.userByLogin(ctx, login)
		if err != nil && !errors.Is(err, errNotFound) {
			return nil, nil, fmt.Errorf("resolving the co-author %s: %w\nIf GitHub's API was erroring, re-running the job is enough", email, err)
		}
		if err != nil || p.ID == 0 {
			unknown = append(unknown, email)
			continue
		}
		found = append(found, p)
	}
	return found, unknown, nil
}

// writeSummary is best-effort: the summary is presentation, and the verdict is
// already carried by the exit code and the annotation.
func (c *checker) writeSummary(ctx context.Context, markdown string) {
	if c.cfg.summaryPath == "" {
		return
	}
	f, err := os.OpenFile(c.cfg.summaryPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		slog.ErrorContext(ctx, "could not open the job summary", slog.String("path", c.cfg.summaryPath), errAttr(err))
		return
	}
	defer func() {
		// A write to the summary is only reported at close on some filesystems,
		// so discarding this would hide the truncation it stands for.
		if cerr := f.Close(); cerr != nil {
			slog.ErrorContext(ctx, "could not close the job summary", slog.String("path", c.cfg.summaryPath), errAttr(cerr))
		}
	}()
	if _, err := fmt.Fprint(f, markdown); err != nil {
		slog.ErrorContext(ctx, "could not write the job summary", slog.String("path", c.cfg.summaryPath), errAttr(err))
	}
}
