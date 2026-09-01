package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, h http.HandlerFunc) *client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := newClient("token", "pug-sh/pug")
	c.baseURL = srv.URL
	return c
}

// The 250-commit cap is only detectable if the walk itself is complete, so the
// Link header has to be followed rather than the first page trusted.
func TestPullCommitsWalksEveryPage(t *testing.T) {
	var base string
	page := 0
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		page++
		if page == 1 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/next>; rel="next"`, base))
			fmt.Fprint(w, `[{"sha":"a1"}]`)
			return
		}
		fmt.Fprint(w, `[{"sha":"b2"}]`)
	})
	base = c.baseURL

	commits, err := c.pullCommits(t.Context(), 7)
	if err != nil {
		t.Fatalf("pullCommits: %v", err)
	}
	if len(commits) != 2 || commits[0].SHA != "a1" || commits[1].SHA != "b2" {
		t.Fatalf("want both pages in order, got %+v", commits)
	}
}

// A base branch that has never had a signature file is the first-contributor
// case, not a failure: it must be distinguishable from every other error.
func TestSignatureFileReportsAMissingFileAsNotFound(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := c.signatureFile(t.Context(), "base"); !errors.Is(err, errNotFound) {
		t.Fatalf("want errNotFound, got %v", err)
	}
}

func TestGetSurfacesAnUnexpectedStatus(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "rate limit exceeded")
	})
	_, err := c.pullCommits(t.Context(), 7)
	if err == nil {
		t.Fatal("want an error for a 403")
	}
	if errors.Is(err, errNotFound) {
		t.Fatalf("a 403 must not read as a missing file: %v", err)
	}
	for _, want := range []string{"403", "rate limit exceeded"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q in the error, got %v", want, err)
		}
	}
}

// userByLogin is the whole of co-author identity now, and resolveCoauthors keys
// its unknown-vs-error split on the errNotFound this returns.
func TestUserByLoginResolvesAndReportsAMissingAccount(t *testing.T) {
	var got string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		if strings.Contains(r.URL.Path, "ghost") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"id":99,"login":"alice","type":"User"}`)
	})

	p, err := c.userByLogin(t.Context(), "alice")
	if err != nil || p.ID != 99 || p.Login != "alice" {
		t.Fatalf("want alice resolved, got %+v %v", p, err)
	}
	if got != "/users/alice" {
		t.Fatalf("want the users endpoint, got %q", got)
	}

	if _, err := c.userByLogin(t.Context(), "ghost"); !errors.Is(err, errNotFound) {
		t.Fatalf("want errNotFound for a missing account, got %v", err)
	}

	// A bot co-author reaches here by design: noreplyRe keeps the [bot] suffix.
	if _, err := c.userByLogin(t.Context(), "dependabot[bot]"); err != nil {
		t.Fatalf("want the login path-escaped, got %v", err)
	}
	if got != "/users/dependabot[bot]" {
		t.Fatalf("want the bot login escaped onto the path, got %q", got)
	}
}

// The merge base is what keeps a stale branch from reading as one that deleted
// the signatures merged since it branched.
func TestMergeBaseReadsTheCompareEndpoint(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/compare/base...head") {
			t.Errorf("want the compare endpoint, got %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"merge_base_commit":{"sha":"mb1"}}`)
	})
	got, err := c.mergeBase(t.Context(), "base", "head")
	if err != nil || got != "mb1" {
		t.Fatalf("want mb1, got %q err=%v", got, err)
	}
}

// A compare response without a merge base must fail rather than silently read the
// signature file at the empty ref, which resolves to the default branch.
func TestMergeBaseRefusesAnEmptyResult(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{}`)
	})
	if _, err := c.mergeBase(t.Context(), "base", "head"); err == nil {
		t.Fatal("want an error when no merge base comes back")
	}
}

// The comment endpoints are the gate's only writes; a wrong method or path is
// silent at compile time and only shows up as a comment nobody ever receives.
func TestCommentWritesUseTheIssueEndpoints(t *testing.T) {
	var method, path, body string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":1}`)
	})

	if err := c.createComment(t.Context(), 7, "please sign"); err != nil {
		t.Fatalf("createComment: %v", err)
	}
	if method != http.MethodPost || path != "/repos/pug-sh/pug/issues/7/comments" {
		t.Fatalf("want a POST to the pull request's comments, got %s %s", method, path)
	}
	if !strings.Contains(body, `"body":"please sign"`) {
		t.Fatalf("want the comment body in the payload, got %s", body)
	}

	if err := c.updateComment(t.Context(), 42, "signed"); err != nil {
		t.Fatalf("updateComment: %v", err)
	}
	if method != http.MethodPatch || path != "/repos/pug-sh/pug/issues/comments/42" {
		t.Fatalf("want a PATCH to the comment itself, got %s %s", method, path)
	}
}

// Stopping at page one would hide the gate's own comment on a busy pull request,
// so every push would post another one instead of editing it.
func TestCommentsWalksEveryPage(t *testing.T) {
	var base string
	page := 0
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		page++
		if page == 1 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/page2>; rel="next"`, base))
			fmt.Fprint(w, `[{"id":1,"body":"first"}]`)
			return
		}
		fmt.Fprint(w, `[{"id":2,"body":"second"}]`)
	})
	base = c.baseURL

	all, err := c.comments(t.Context(), 7)
	if err != nil {
		t.Fatalf("comments: %v", err)
	}
	if len(all) != 2 || all[0].ID != 1 || all[1].ID != 2 {
		t.Fatalf("want both pages, got %+v", all)
	}
}

func TestSendSurfacesARefusedWrite(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "resource not accessible by integration")
	})
	err := c.createComment(t.Context(), 7, "please sign")
	if err == nil || !strings.Contains(err.Error(), "resource not accessible") {
		t.Fatalf("want the refusal surfaced, got %v", err)
	}
}

// The label name carries a space and a colon, so removing it goes through a
// path-escaped URL rather than a formatted one.
func TestLabelWritesUseTheIssueEndpoints(t *testing.T) {
	var method, path, body string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.EscapedPath()
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		fmt.Fprint(w, `[]`)
	})

	if _, err := c.labels(t.Context(), 7); err != nil {
		t.Fatalf("labels: %v", err)
	}
	if method != http.MethodGet || path != "/repos/pug-sh/pug/issues/7/labels" {
		t.Fatalf("want a GET of the pull request's labels, got %s %s", method, path)
	}

	if err := c.addLabel(t.Context(), 7, labelUnsigned); err != nil {
		t.Fatalf("addLabel: %v", err)
	}
	if method != http.MethodPost || path != "/repos/pug-sh/pug/issues/7/labels" {
		t.Fatalf("want a POST to the pull request's labels, got %s %s", method, path)
	}
	if !strings.Contains(body, `{"labels":["cla: not signed"]}`) {
		t.Fatalf("want the label in the payload, got %s", body)
	}

	if err := c.removeLabel(t.Context(), 7, labelUnsigned); err != nil {
		t.Fatalf("removeLabel: %v", err)
	}
	if method != http.MethodDelete || path != "/repos/pug-sh/pug/issues/7/labels/cla:%20not%20signed" {
		t.Fatalf("want an escaped DELETE to the label itself, got %s %s", method, path)
	}
	if body != "" {
		t.Fatalf("a delete must not carry a body, got %q", body)
	}
}

// A stale blob sha is a signature that landed between the read and the write, not
// a fault, so it has to be distinguishable from every other non-2xx.
func TestSendReportsConflictAsErrConflict(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"message":"does not match abc123"}`)
	})
	err := c.send(t.Context(), http.MethodPut, c.baseURL+"/x", map[string]string{"a": "b"})
	if !errors.Is(err, errConflict) {
		t.Fatalf("send on 409 = %v, want errConflict", err)
	}
}

func TestSendStillReportsOtherFailures(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"forbidden"}`)
	})
	err := c.send(t.Context(), http.MethodPut, c.baseURL+"/x", nil)
	if err == nil || errors.Is(err, errConflict) {
		t.Fatalf("send on 403 = %v, want a plain error", err)
	}
}

// GitHub wraps base64 content at 60 characters. A decoder that does not strip the
// newlines fails on every real response, so the fixture carries one.
func TestSignatureFileMetaDecodesContentAndSHA(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("ref"); got != "main" {
			t.Errorf("ref = %q, want main", got)
		}
		fmt.Fprint(w, `{"sha":"abc123","encoding":"base64","content":"eyJjbGFfdmVyc2lvbiI6InYxIiwic2ln\nbmF0dXJlcyI6W119"}`)
	})
	f, sha, err := c.signatureFileMeta(t.Context(), "main")
	if err != nil {
		t.Fatalf("signatureFileMeta: %v", err)
	}
	if sha != "abc123" {
		t.Errorf("sha = %q, want abc123", sha)
	}
	if f.CLAVersion != "v1" {
		t.Errorf("cla_version = %q, want v1", f.CLAVersion)
	}
	if f.Signatures == nil {
		t.Error("signatures decoded as nil, want an empty slice")
	}
}

func TestSignatureFileMetaRejectsAnUnexpectedEncoding(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"sha":"abc123","encoding":"none","content":""}`)
	})
	if _, _, err := c.signatureFileMeta(t.Context(), "main"); err == nil {
		t.Fatal("signatureFileMeta accepted a non-base64 encoding")
	}
}
