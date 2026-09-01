package main

import (
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
