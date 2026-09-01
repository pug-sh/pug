package main

import (
	"errors"
	"testing"
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
