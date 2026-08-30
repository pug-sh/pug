// Command clacheck gates a pull request on the Contributor License Agreement.
//
// Everyone whose copyright can reach the repository through the pull request —
// commit author, committer, co-author, and the person who opened it — must appear
// in signatures/cla.json. The file is read from the pull request's own head, so a
// contributor signs in the same pull request; the edit must be append-only and may
// only add people who authored one of its commits.
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
	}
	return c, nil
}

// githubAPI is the GitHub surface the gate depends on, named consumer-side so
// the check can be unit-tested against a fake instead of the live API. Every
// implementation must report a missing resource as errNotFound: check reads it
// as the first-contributor case rather than a failure.
type githubAPI interface {
	signatureFile(ctx context.Context, ref string) (*SignatureFile, error)
	pullCommits(ctx context.Context, pr int) ([]Commit, error)
	mergeBase(ctx context.Context, base, head string) (string, error)
	userByLogin(ctx context.Context, login string) (Principal, error)
	userByEmail(ctx context.Context, email string) (Principal, error)
}

// checker is everything the gate talks to. run assembles it once so check itself
// reaches for no ambient state — not the API, not stdout, not the clock.
type checker struct {
	cfg config
	gh  githubAPI
	out io.Writer
	now func() time.Time
}

func main() {
	setupLogging()

	// An unsigned CLA has already been reported with its own annotation; anything
	// else is a checker fault that nothing has annotated yet.
	switch err := run(context.Background()); {
	case errors.Is(err, errUnsigned):
		os.Exit(1)
	case err != nil:
		fmt.Printf("::error::%s\n", err)
		os.Exit(1)
	}
}

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

func (c *checker) check(ctx context.Context) error {
	slog.InfoContext(ctx, "checking signatures",
		slog.String("repo", c.cfg.repo), slog.Int("pr", c.cfg.pr), slog.String("head", c.cfg.headSHA))

	head, err := c.gh.signatureFile(ctx, c.cfg.headSHA)
	if err != nil {
		return fmt.Errorf("reading signatures/cla.json at the pull request head: %w", err)
	}
	if err := head.validate(); err != nil {
		return fmt.Errorf("signatures/cla.json is invalid: %w", err)
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

	coauthors, unresolved := c.resolveCoauthors(ctx, commits)
	if len(unresolved) > 0 {
		return fmt.Errorf("these Co-authored-by addresses did not resolve to a GitHub account: %s\n"+
			"A co-author holds copyright and must sign too; use their <id>+<login>@users.noreply.github.com address or drop the trailer. If GitHub's API was erroring, re-running the job is enough",
			strings.Join(unresolved, ", "))
	}
	people = append(people, coauthors...)

	base, err := c.baseFile(ctx)
	if err != nil {
		return err
	}
	prIDs := make(map[int64]bool, len(people))
	for _, p := range people {
		prIDs[p.ID] = true
	}
	if err := appendOnly(base, head, prIDs); err != nil {
		return err
	}

	missing, checked := unsigned(head, people)
	if len(missing) > 0 {
		report := unsignedReport(c.cfg, head.CLAVersion, missing, c.now())
		fmt.Fprint(c.out, report.text)
		c.writeSummary(ctx, report.markdown)
		return errUnsigned
	}
	// A pull request authored entirely by bots — dependabot and friends — has no
	// human copyright to license, so there is nothing to sign for.
	if len(checked) == 0 {
		fmt.Fprintf(c.out, "CLA %s: no human authors across %d commit(s); nothing to sign\n", head.CLAVersion, len(commits))
		return nil
	}

	logins := make([]string, len(checked))
	for i, p := range checked {
		logins[i] = p.Login
	}
	fmt.Fprintf(c.out, "CLA %s verified for %d principal(s) across %d commit(s): %s\n",
		head.CLAVersion, len(checked), len(commits), strings.Join(logins, ", "))
	return nil
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
	switch {
	case errors.Is(err, errNotFound):
		return &SignatureFile{Signatures: []Signature{}}, nil
	case err != nil:
		return nil, fmt.Errorf("reading signatures/cla.json at %s: %w", mergeBase, err)
	}
	return base, nil
}

// resolveCoauthors turns Co-authored-by trailers into principals. A trailer is
// commit-message text, so nothing in it is taken on trust — the address only
// chooses which lookup runs, and the id always comes back from the API.
func (c *checker) resolveCoauthors(ctx context.Context, commits []Commit) (found []Principal, unresolved []string) {
	for _, email := range coauthorEmails(commits) {
		var (
			p   Principal
			err error
		)
		if login := noreplyLogin(email); login != "" {
			p, err = c.gh.userByLogin(ctx, login)
		} else {
			p, err = c.gh.userByEmail(ctx, email)
		}
		if err != nil && !errors.Is(err, errNotFound) {
			// The contributor is told the same thing either way; only the log
			// separates "no such account" from a rate limit or an outage.
			slog.WarnContext(ctx, "co-author lookup failed", slog.String("email", email), errAttr(err))
		}
		if err != nil || p.ID == 0 {
			unresolved = append(unresolved, email)
			continue
		}
		found = append(found, p)
	}
	return found, unresolved
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
