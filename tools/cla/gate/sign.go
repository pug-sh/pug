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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// signWorkflowFile is the checker's workflow, re-run once a signature lands so the
// pull request's own required check turns green rather than a second check
// merely agreeing with it.
const signWorkflowFile = "cla.yaml"

// signCommand is the whole comment, not a prefix of it. The workflow gates on a
// prefix because Actions expressions cannot trim, so this is the authoritative
// match.
const signCommand = "/sign"

type signConfig struct {
	repo       string
	pr         int
	baseBranch string
	commenter  Principal
	serverURL  string
	token      string
}

func loadSignConfig() (signConfig, error) {
	c := signConfig{
		repo:       os.Getenv("GITHUB_REPOSITORY"),
		baseBranch: env("BASE_BRANCH", "main"),
		serverURL:  env("GITHUB_SERVER_URL", "https://github.com"),
		token:      env("GH_TOKEN", os.Getenv("GITHUB_TOKEN")),
	}
	var err error
	if c.pr, err = strconv.Atoi(os.Getenv("PR_NUMBER")); err != nil {
		return c, fmt.Errorf("PR_NUMBER: %w", err)
	}
	id, err := strconv.ParseInt(os.Getenv("COMMENTER_ID"), 10, 64)
	if err != nil {
		return c, fmt.Errorf("COMMENTER_ID: %w", err)
	}
	c.commenter = Principal{ID: id, Login: os.Getenv("COMMENTER_LOGIN"), Type: os.Getenv("COMMENTER_TYPE")}
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
	// No "User" fallback: that is the value isBot() reads as human, so a renamed
	// payload field would sign for a bot rather than refuse one.
	case c.commenter.Type == "":
		return c, errors.New("COMMENTER_TYPE is empty")
	case c.pr <= 0:
		return c, fmt.Errorf("PR_NUMBER is %d", c.pr)
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

// errDeclined marks a refusal decline has already replied to, so sign does not
// post a second, vaguer comment on top of the specific one.
var errDeclined = errors.New("declined")

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

// putRetries is one retry, not a loop. A conflict means another signature landed;
// a second one in the time it takes to re-read is not congestion but a bug, and
// spinning on it would hold the runner while making it worse.
const putRetries = 1

// rerunFailed is advice, not an apology: the signature is committed either way, so
// what the contributor needs is the one action that gets their check re-run.
const rerunFailed = "\n\nThe CLA check could not be re-run automatically — push any commit, or ask a maintainer to re-run it."

// stillRed is the same advice for someone whose signature was already on file: a
// repeat /sign is usually an attempt to clear a check that never got re-run.
const stillRed = "\n\nIf the CLA check is still red, push any commit or ask a maintainer to re-run it."

type signer struct {
	cfg signConfig
	gh  githubAPI
	now func() time.Time
}

// A contributor who typed /sign must be answered. An issue_comment run attaches
// to no check on the pull request, so an error that only annotates the job is a
// comment nobody replied to.
func (s *signer) sign(ctx context.Context) error {
	err := s.record(ctx)
	if err != nil && !errors.Is(err, errDeclined) {
		s.reply(ctx, fmt.Sprintf("@%s — `/sign` could not be recorded: %s", s.cfg.commenter.Login, err))
	}
	return err
}

func (s *signer) record(ctx context.Context) error {
	pr, err := s.gh.pullRequest(ctx, s.cfg.pr)
	if err != nil {
		return fmt.Errorf("reading pull request %d: %w", s.cfg.pr, err)
	}
	slog.InfoContext(ctx, "recording a signature",
		slog.String("repo", s.cfg.repo), slog.Int("pr", s.cfg.pr),
		slog.String("commenter", s.cfg.commenter.Login), slog.String("base", pr.Base.Ref))

	// issue_comment carries no branch filter, so the target is checked here: a
	// signature committed to any other branch is one the checker never reads, and
	// the contributor would be told it landed.
	if pr.Base.Ref != s.cfg.baseBranch {
		return s.decline(ctx, fmt.Errorf("signatures are recorded on %s only, and this pull request targets %s", s.cfg.baseBranch, pr.Base.Ref))
	}
	// issue_comment fires on a closed pull request too, and a merged one's head is
	// gone once the fork is deleted.
	if pr.State != "open" {
		return s.decline(ctx, fmt.Errorf("this pull request is %s; sign on an open one", pr.State))
	}

	commits, err := s.gh.pullCommits(ctx, s.cfg.pr)
	if err != nil {
		return fmt.Errorf("listing commits: refusing to sign against an unverified list: %w", err)
	}
	// The checker's own 250-cap guard. A truncated list here would drop principals
	// and refuse a contributor who really is one.
	if len(commits) != pr.Commits {
		return fmt.Errorf("listed %d of the pull request's %d commits, so a principal could be missed; GitHub caps that endpoint at 250, so squash or split a pull request that large, otherwise re-run the job",
			len(commits), pr.Commits)
	}

	// An unlinked commit drops its author from people entirely, so a contributor
	// who has commits here would otherwise be refused for having none.
	people, unlinked := principals(commits, pr.User)
	coauthors, _, err := resolveCoauthors(ctx, s.gh, commits)
	if err != nil {
		return err
	}
	people = append(people, coauthors...)

	head, err := s.gh.signatureFile(ctx, pr.Head.SHA)
	if err != nil {
		return fmt.Errorf("reading %s at the pull request head: %w", signaturesPath, err)
	}

	for attempt := 0; ; attempt++ {
		onBase, sha, err := s.gh.signatureFileMeta(ctx, pr.Base.Ref)
		if err != nil {
			return fmt.Errorf("reading %s on %s: %w", signaturesPath, pr.Base.Ref, err)
		}
		if err := maySign(s.cfg.commenter, people, head, onBase, onBase.CLAVersion); err != nil {
			if errors.Is(err, errNotAPrincipal) && len(unlinked) > 0 {
				err = fmt.Errorf("%w; these commits carry an email that is not linked to a GitHub account, so their author cannot be identified: %s", err, strings.Join(unlinked, ", "))
			}
			return s.decline(ctx, err)
		}

		onBase.Signatures = append(onBase.Signatures, Signature{
			Login: s.cfg.commenter.Login,
			ID:    s.cfg.commenter.ID,
			Date:  s.now().UTC().Format(time.DateOnly),
			CLA:   onBase.CLAVersion,
		})
		// Validate what is about to be written, not what was read: the signer must
		// never be the thing that makes the file unparseable for everyone else.
		if err := onBase.validate(); err != nil {
			return fmt.Errorf("the signature would make %s invalid: %w", signaturesPath, err)
		}

		msg := fmt.Sprintf("chore(cla): sign %s for @%s", onBase.CLAVersion, s.cfg.commenter.Login)
		err = s.gh.putSignatureFile(ctx, pr.Base.Ref, onBase, sha, msg, s.cfg.commenter)
		switch {
		case err == nil:
			return s.confirm(ctx, pr, onBase.CLAVersion)
		case errors.Is(err, errConflict) && attempt < putRetries:
			slog.InfoContext(ctx, "another signature landed first; re-reading", slog.Int("attempt", attempt+1))
		default:
			return fmt.Errorf("committing the signature to %s: %w", pr.Base.Ref, err)
		}
	}
}

// reply is how the contributor learns what happened. Best-effort: failing the job
// over a comment would misreport a signature that did land. It annotates too,
// because the run that most needs this seen exits 0 and nobody opens a green log.
func (s *signer) reply(ctx context.Context, body string) {
	if err := s.gh.createComment(ctx, s.cfg.pr, body); err != nil {
		slog.ErrorContext(ctx, "could not post the reply", errAttr(err))
		fmt.Printf("::warning::%s\n", escapeAnnotation("could not post the reply: "+err.Error()))
	}
}

// confirm tells the contributor the signature landed and re-runs the checker, so
// the pull request's own required check turns green rather than a second one
// agreeing with it. The re-run is best-effort: the signature is committed either
// way, and failing here would report a signing that happened as one that did not.
func (s *signer) confirm(ctx context.Context, pr PullRequest, version string) error {
	body := fmt.Sprintf("Signed CLA **%s** for @%s — recorded in `%s` on `%s`. Thanks!",
		version, s.cfg.commenter.Login, signaturesPath, pr.Base.Ref)

	switch run, err := s.gh.latestWorkflowRun(ctx, signWorkflowFile, pr.Head.SHA); {
	case errors.Is(err, errNotFound):
		body += "\n\nNo CLA check has run here yet; the first one will pass."
	case err != nil:
		slog.ErrorContext(ctx, "could not find the CLA run to re-run", errAttr(err))
		body += rerunFailed
	default:
		if err := s.gh.rerunWorkflow(ctx, run.ID); err != nil {
			slog.ErrorContext(ctx, "could not re-run the CLA check", errAttr(err))
			body += rerunFailed
		}
	}

	s.reply(ctx, body)
	return nil
}

// decline reports why no signature was recorded.
//
// Already-signed is not a failure: a contributor commenting twice, or one whose
// first /sign landed before the check re-ran, already has what they came for, so
// the run says so and exits green. Every other reason fails the job, which is the
// only place an operator can see that a /sign was refused.
func (s *signer) decline(ctx context.Context, reason error) error {
	body := fmt.Sprintf("@%s — %s.", s.cfg.commenter.Login, reason)
	settled := errors.Is(reason, errAlreadySigned)
	if settled {
		body += stillRed
	} else {
		body += fmt.Sprintf("\n\nSee [CLA.md](%s/%s/blob/HEAD/CLA.md) for how signing works.", s.cfg.serverURL, s.cfg.repo)
	}
	s.reply(ctx, body)
	if settled {
		slog.InfoContext(ctx, "nothing to record", slog.String("commenter", s.cfg.commenter.Login))
		return nil
	}
	return fmt.Errorf("refused to sign for %s: %w: %w", s.cfg.commenter.Login, errDeclined, reason)
}

func runSign(ctx context.Context) error {
	// A near-miss exits quietly rather than replying: the contributor meant
	// something else, and a refusal posted under every "I'll /sign later" would be
	// worse than saying nothing.
	if body := strings.TrimSpace(os.Getenv("COMMENT_BODY")); body != signCommand {
		slog.InfoContext(ctx, "comment is not the sign command; nothing to do")
		return nil
	}

	cfg, err := loadSignConfig()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	s := &signer{cfg: cfg, gh: newClient(cfg.token, cfg.repo), now: time.Now}
	return s.sign(ctx)
}
