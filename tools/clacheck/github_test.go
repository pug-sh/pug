package main

import (
	"errors"
	"fmt"
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

// userByEmail deliberately refuses an ambiguous match rather than picking one.
func TestUserByEmailRefusesAnAmbiguousMatch(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"items":[{"id":1,"login":"a"},{"id":2,"login":"b"}]}`)
	})
	if _, err := c.userByEmail(t.Context(), "shared@example.com"); !errors.Is(err, errNotFound) {
		t.Fatalf("want errNotFound for two matches, got %v", err)
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
