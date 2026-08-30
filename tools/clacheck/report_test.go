package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func reportCfg() config {
	return config{repo: "pug-sh/pug", baseRef: "main", serverURL: "https://github.com"}
}

func TestUnsignedReportFillsInTheContributorsEntry(t *testing.T) {
	r := unsignedReport(reportCfg(), "v1", []Principal{{ID: 42, Login: "carol", Type: "User"}}, fixedNow)

	if !strings.HasPrefix(r.text, "::error::CLA v1 not signed by: carol\n") {
		t.Fatalf("the annotation must be the first line, got %q", r.text)
	}
	for _, want := range []string{`"login": "carol"`, `"id":    42`, `"date":  "2026-08-30"`, `"cla":   "v1"`} {
		if !strings.Contains(r.text, want) {
			t.Errorf("want %q in the log report", want)
		}
		if !strings.Contains(r.markdown, want) {
			t.Errorf("want %q in the job summary", want)
		}
	}
	claURL := "https://github.com/pug-sh/pug/blob/main/CLA.md"
	if !strings.Contains(r.text, claURL) || !strings.Contains(r.markdown, claURL) {
		t.Errorf("both halves must link the agreement at %s", claURL)
	}
}

// The summary's code block is meant to be pasted straight into the signatures
// array, so it has to parse as array elements — not just look like JSON.
func TestUnsignedReportEmitsPasteableJSON(t *testing.T) {
	r := unsignedReport(reportCfg(), "v1", []Principal{
		{ID: 42, Login: "carol", Type: "User"},
		{ID: 7, Login: "dave", Type: "User"},
	}, fixedNow)

	_, block, ok := strings.Cut(r.markdown, "```json\n")
	if !ok {
		t.Fatalf("want a json block in the summary, got %q", r.markdown)
	}
	block, _, ok = strings.Cut(block, "\n```")
	if !ok {
		t.Fatalf("want the json block to be closed, got %q", r.markdown)
	}

	var entries []Signature
	if err := json.Unmarshal([]byte("["+block+"]"), &entries); err != nil {
		t.Fatalf("the block does not paste into the signatures array: %v\n%s", err, block)
	}
	if len(entries) != 2 || entries[0].ID != 42 || entries[1].ID != 7 {
		t.Fatalf("want both entries with their ids, got %+v", entries)
	}
	for _, e := range entries {
		if e.Name != "Your Name" || e.CLA != "v1" || e.Date != "2026-08-30" {
			t.Fatalf("entry is not ready to edit and commit: %+v", e)
		}
	}
}

func TestPlural(t *testing.T) {
	if got := plural(1, "this entry", "these entries"); got != "this entry" {
		t.Fatalf("want the singular, got %q", got)
	}
	if got := plural(2, "this entry", "these entries"); got != "these entries" {
		t.Fatalf("want the plural, got %q", got)
	}
}
