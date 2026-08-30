package main

import (
	"bytes"
	"context"
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

// signatureFile reads signatures/cla.json at a given ref over the API, so the
// gate never depends on a checkout of the pull request's own tree.
func (c *client) signatureFile(ctx context.Context, ref string) (*SignatureFile, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/contents/signatures/cla.json?ref=%s",
		c.baseURL, c.repo, url.QueryEscape(ref))
	body, _, err := c.get(ctx, endpoint, "application/vnd.github.raw")
	if err != nil {
		return nil, err
	}
	var f SignatureFile
	if err := json.Unmarshal(body, &f); err != nil {
		slog.ErrorContext(ctx, "signatures/cla.json did not decode", slog.String("ref", ref), errAttr(err))
		return nil, fmt.Errorf("signatures/cla.json is not valid JSON: %w", err)
	}
	return &f, nil
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

// userByEmail only accepts an unambiguous match. Anything else is reported as
// unresolved so the contributor is asked, rather than the check quietly passing.
func (c *client) userByEmail(ctx context.Context, email string) (Principal, error) {
	endpoint := c.baseURL + "/search/users?q=" + url.QueryEscape(email+" in:email")
	body, _, err := c.get(ctx, endpoint, "application/vnd.github+json")
	if err != nil {
		return Principal{}, err
	}
	var res struct {
		Items []Principal `json:"items"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		slog.ErrorContext(ctx, "an email search did not decode", errAttr(err))
		return Principal{}, err
	}
	if len(res.Items) != 1 {
		slog.DebugContext(ctx, "email search was not unambiguous", slog.Int("matches", len(res.Items)))
		return Principal{}, errNotFound
	}
	return res.Items[0], nil
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
