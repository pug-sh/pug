package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var errNotFound = errors.New("not found")

// errConflict is the contents API refusing a write whose blob sha is stale, which
// means a signature landed between our read and our write rather than that
// anything failed. The caller re-reads and retries; every other non-2xx is
// terminal.
var errConflict = errors.New("conflict")

// errNoRuns is an empty run list, which is not a missing resource: a contributor
// can comment /sign before the checker has ever run. Kept apart from errNotFound
// so a 404 — a renamed workflow file — cannot be reported as "the first one will
// pass" on a check that will never run.
var errNoRuns = errors.New("no run yet")

const defaultBaseURL = "https://api.github.com"

type client struct {
	http    *http.Client
	token   string
	repo    string
	baseURL string
}

func newClient(token, repo string) *client {
	return &client{
		http:    &http.Client{Timeout: 30 * time.Second},
		token:   token,
		repo:    repo,
		baseURL: defaultBaseURL,
	}
}

// get reports its own failures: a 5xx and a rate limit reach the caller as the
// same "could not resolve" outcome, and the log is what tells them apart.
func (c *client) get(ctx context.Context, endpoint, accept string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	slog.DebugContext(ctx, "github request", slog.String("endpoint", endpoint))

	resp, err := c.http.Do(req)
	if err != nil {
		slog.ErrorContext(ctx, "github request failed", slog.String("endpoint", endpoint), errAttr(err))
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.ErrorContext(ctx, "reading the github response failed", slog.String("endpoint", endpoint), errAttr(err))
		return nil, nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		slog.DebugContext(ctx, "github reported not found", slog.String("endpoint", endpoint))
		return nil, nil, errNotFound
	case resp.StatusCode != http.StatusOK:
		err := fmt.Errorf("GET %s: %s: %s", endpoint, resp.Status, strings.TrimSpace(string(body)))
		slog.ErrorContext(ctx, "github returned an unexpected status",
			slog.String("endpoint", endpoint),
			slog.Int("status", resp.StatusCode),
			slog.String("rate_limit_remaining", resp.Header.Get("X-RateLimit-Remaining")),
			errAttr(err))
		return nil, nil, err
	}
	return body, resp.Header, nil
}

// signaturesPath is the one place the file's location is written down. The gate
// reads it two ways — raw for a decision, with metadata for a write — and a
// disagreement between them would read one file and overwrite another.
const signaturesPath = "tools/cla/signatures.json"

// signatureFile reads tools/cla/signatures.json at a given ref over the API, so the
// gate never depends on a checkout of the pull request's own tree.
func (c *client) signatureFile(ctx context.Context, ref string) (*SignatureFile, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s",
		c.baseURL, c.repo, signaturesPath, url.QueryEscape(ref))
	body, _, err := c.get(ctx, endpoint, "application/vnd.github.raw")
	if err != nil {
		return nil, err
	}
	var f SignatureFile
	if err := json.Unmarshal(body, &f); err != nil {
		slog.ErrorContext(ctx, "tools/cla/signatures.json did not decode", slog.String("ref", ref), errAttr(err))
		return nil, fmt.Errorf("tools/cla/signatures.json is not valid JSON: %w", err)
	}
	return &f, nil
}

// signatureFileMeta reads the same file as JSON rather than raw, for the blob sha.
// That sha is what makes the signer's write conditional: without it a signature
// landing between the read and the write is silently overwritten instead of
// rejected, and the loser never learns their signature is gone.
func (c *client) signatureFileMeta(ctx context.Context, ref string) (*SignatureFile, string, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s",
		c.baseURL, c.repo, signaturesPath, url.QueryEscape(ref))
	body, _, err := c.get(ctx, endpoint, "application/vnd.github+json")
	if err != nil {
		return nil, "", err
	}
	var meta struct {
		SHA      string `json:"sha"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		slog.ErrorContext(ctx, "the contents response did not decode", slog.String("ref", ref), errAttr(err))
		return nil, "", fmt.Errorf("the contents response for %s did not decode: %w", signaturesPath, err)
	}
	if meta.Encoding != "base64" {
		return nil, "", fmt.Errorf("the contents response for %s is %q-encoded, not base64", signaturesPath, meta.Encoding)
	}
	// GitHub wraps the encoding at 60 characters, and the decoder rejects newlines.
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(meta.Content, "\n", ""))
	if err != nil {
		slog.ErrorContext(ctx, "the contents response did not decode from base64", slog.String("ref", ref), errAttr(err))
		return nil, "", fmt.Errorf("the contents response for %s did not decode from base64: %w", signaturesPath, err)
	}
	var f SignatureFile
	if err := json.Unmarshal(raw, &f); err != nil {
		slog.ErrorContext(ctx, "tools/cla/signatures.json did not decode", slog.String("ref", ref), errAttr(err))
		return nil, "", fmt.Errorf("%s is not valid JSON: %w", signaturesPath, err)
	}
	return &f, meta.SHA, nil
}

// noreplyEmail is the address GitHub itself writes for a commit made through the
// web UI, and the only address form the gate's own trailer resolution accepts.
func noreplyEmail(p Principal) string {
	return fmt.Sprintf("%d+%s@users.noreply.github.com", p.ID, p.Login)
}

// marshalSignatureFile reproduces the file's on-disk shape — two-space indent,
// trailing newline — so a signature recorded by /sign leaves no reformatting diff
// against one added by hand, and the next hand-edit does not rewrite the file.
func marshalSignatureFile(f *SignatureFile) ([]byte, error) {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// putSignatureFile commits the file to branch. The contributor is the commit
// author and the workflow's token is the committer, so the record lives in git
// history under the identity that agreed to it rather than the bot's. sha makes
// the write conditional; a stale one comes back as errConflict.
func (c *client) putSignatureFile(ctx context.Context, branch string, f *SignatureFile, sha, message string, author Principal) error {
	content, err := marshalSignatureFile(f)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/contents/%s", c.baseURL, c.repo, signaturesPath)
	return c.send(ctx, http.MethodPut, endpoint, map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"sha":     sha,
		"branch":  branch,
		"author":  map[string]string{"name": author.Login, "email": noreplyEmail(author)},
	})
}

// PullRequest is the part of the pull request the signer needs and the
// issue_comment payload does not carry: where to commit, and how many commits to
// expect so a list truncated at GitHub's 250 cap is caught rather than silently
// under-reporting who has work here.
type PullRequest struct {
	Number  int       `json:"number"`
	State   string    `json:"state"`
	Commits int       `json:"commits"`
	User    Principal `json:"user"`
	Head    struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (c *client) pullRequest(ctx context.Context, pr int) (PullRequest, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/pulls/%d", c.baseURL, c.repo, pr)
	body, _, err := c.get(ctx, endpoint, "application/vnd.github+json")
	if err != nil {
		return PullRequest{}, err
	}
	var out PullRequest
	if err := json.Unmarshal(body, &out); err != nil {
		slog.ErrorContext(ctx, "the pull request response did not decode", slog.Int("pr", pr), errAttr(err))
		return PullRequest{}, fmt.Errorf("the pull request response did not decode: %w", err)
	}
	return out, nil
}

// WorkflowRun is one run of the checker's workflow. The signer re-runs an existing
// run rather than dispatching a fresh one: only pull_request_target runs attach to
// a pull request's checks, so a workflow_dispatch would execute happily and change
// nothing the merge button can see.
type WorkflowRun struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

func (c *client) latestWorkflowRun(ctx context.Context, workflowFile, headSHA string) (WorkflowRun, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/actions/workflows/%s/runs?head_sha=%s&per_page=1",
		c.baseURL, c.repo, url.PathEscape(workflowFile), url.QueryEscape(headSHA))
	body, _, err := c.get(ctx, endpoint, "application/vnd.github+json")
	if err != nil {
		return WorkflowRun{}, err
	}
	var page struct {
		Runs []WorkflowRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		slog.ErrorContext(ctx, "the workflow runs response did not decode", errAttr(err))
		return WorkflowRun{}, fmt.Errorf("the workflow runs response did not decode: %w", err)
	}
	if len(page.Runs) == 0 {
		return WorkflowRun{}, errNoRuns
	}
	return page.Runs[0], nil
}

func (c *client) rerunWorkflow(ctx context.Context, runID int64) error {
	endpoint := fmt.Sprintf("%s/repos/%s/actions/runs/%d/rerun", c.baseURL, c.repo, runID)
	return c.send(ctx, http.MethodPost, endpoint, nil)
}

var nextPageRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// pullCommits walks every page. GitHub caps this endpoint at 250 commits and
// simply stops sending a next link, so the caller compares the count against the
// pull request's own commit total rather than trusting the walk to be complete.
func (c *client) pullCommits(ctx context.Context, pr int) ([]Commit, error) {
	all, err := listPaged[Commit](ctx, c, fmt.Sprintf("%s/repos/%s/pulls/%d/commits?per_page=100", c.baseURL, c.repo, pr), "commit")
	if err != nil {
		return nil, err
	}
	slog.DebugContext(ctx, "listed commits", slog.Int("pr", pr), slog.Int("commits", len(all)))
	return all, nil
}

// listPaged follows the Link header, so a short read is detectable rather than
// read as a complete list.
func listPaged[T any](ctx context.Context, c *client, endpoint, what string) ([]T, error) {
	var all []T
	for endpoint != "" {
		body, header, err := c.get(ctx, endpoint, "application/vnd.github+json")
		if err != nil {
			return nil, err
		}
		var page []T
		if err := json.Unmarshal(body, &page); err != nil {
			slog.ErrorContext(ctx, "a "+what+" page did not decode", slog.String("endpoint", endpoint), errAttr(err))
			return nil, err
		}
		all = append(all, page...)
		endpoint = ""
		if m := nextPageRe.FindStringSubmatch(header.Get("Link")); m != nil {
			endpoint = m[1]
		}
	}
	return all, nil
}

// mergeBase is the commit the pull request actually branched from. It is not in
// the event payload — base.sha there is the base branch tip, which moves.
func (c *client) mergeBase(ctx context.Context, base, head string) (string, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/compare/%s...%s",
		c.baseURL, c.repo, url.PathEscape(base), url.PathEscape(head))
	body, _, err := c.get(ctx, endpoint, "application/vnd.github+json")
	if err != nil {
		return "", err
	}
	var res struct {
		MergeBaseCommit struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		slog.ErrorContext(ctx, "the compare response did not decode", errAttr(err))
		return "", err
	}
	if res.MergeBaseCommit.SHA == "" {
		return "", errors.New("compare returned no merge base")
	}
	return res.MergeBaseCommit.SHA, nil
}

func (c *client) userByLogin(ctx context.Context, login string) (Principal, error) {
	body, _, err := c.get(ctx, c.baseURL+"/users/"+url.PathEscape(login), "application/vnd.github+json")
	if err != nil {
		return Principal{}, err
	}
	var p Principal
	if err := json.Unmarshal(body, &p); err != nil {
		slog.ErrorContext(ctx, "a user lookup did not decode", slog.String("login", login), errAttr(err))
		return Principal{}, err
	}
	return p, nil
}

// Comment is the gate's own pull request comment, matched by its marker rather
// than by author: the token's identity differs between a GitHub App and Actions.
type Comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// GitHub answers a create with 201 and an edit with 200, so any 2xx counts.
func (c *client) send(ctx context.Context, method, endpoint string, payload any) error {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		slog.ErrorContext(ctx, "github request failed", slog.String("endpoint", endpoint), errAttr(err))
		return err
	}
	defer resp.Body.Close()
	res, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.ErrorContext(ctx, "reading the github response failed", slog.String("endpoint", endpoint), errAttr(err))
		return err
	}
	if resp.StatusCode == http.StatusConflict {
		// The body is what separates a lost race from a refusal that will never
		// succeed, such as a protected branch; errors.Is still matches.
		return fmt.Errorf("%w: %s", errConflict, strings.TrimSpace(string(res)))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err := fmt.Errorf("%s %s: %s: %s", method, endpoint, resp.Status, strings.TrimSpace(string(res)))
		// The rate limit separates a throttled 403 from a permissions 403, which
		// look identical here and have opposite remedies.
		slog.ErrorContext(ctx, "github returned an unexpected status",
			slog.String("endpoint", endpoint), slog.Int("status", resp.StatusCode),
			slog.String("rate_limit_remaining", resp.Header.Get("X-RateLimit-Remaining")), errAttr(err))
		return err
	}
	return nil
}

func (c *client) comments(ctx context.Context, pr int) ([]Comment, error) {
	return listPaged[Comment](ctx, c, fmt.Sprintf("%s/repos/%s/issues/%d/comments?per_page=100", c.baseURL, c.repo, pr), "comment")
}

func (c *client) createComment(ctx context.Context, pr int, body string) error {
	endpoint := fmt.Sprintf("%s/repos/%s/issues/%d/comments", c.baseURL, c.repo, pr)
	return c.send(ctx, http.MethodPost, endpoint, map[string]string{"body": body})
}

func (c *client) updateComment(ctx context.Context, id int64, body string) error {
	endpoint := fmt.Sprintf("%s/repos/%s/issues/comments/%d", c.baseURL, c.repo, id)
	return c.send(ctx, http.MethodPatch, endpoint, map[string]string{"body": body})
}

type Label struct {
	Name string `json:"name"`
}

func (c *client) labels(ctx context.Context, pr int) ([]Label, error) {
	return listPaged[Label](ctx, c, fmt.Sprintf("%s/repos/%s/issues/%d/labels?per_page=100", c.baseURL, c.repo, pr), "label")
}

func (c *client) addLabel(ctx context.Context, pr int, name string) error {
	endpoint := fmt.Sprintf("%s/repos/%s/issues/%d/labels", c.baseURL, c.repo, pr)
	return c.send(ctx, http.MethodPost, endpoint, map[string][]string{"labels": {name}})
}

// The name is escaped, not interpolated: it carries a space and a colon.
func (c *client) removeLabel(ctx context.Context, pr int, name string) error {
	endpoint := fmt.Sprintf("%s/repos/%s/issues/%d/labels/%s", c.baseURL, c.repo, pr, url.PathEscape(name))
	return c.send(ctx, http.MethodDelete, endpoint, nil)
}
