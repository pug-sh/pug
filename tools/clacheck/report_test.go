package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func reportCfg() config {
	return config{repo: "pug-sh/pug", baseRef: "main", serverURL: "https://github.com"}
}

func TestUnsignedReportFillsInTheContributorsEntry(t *testing.T) {
	r := unsignedReport(reportCfg(), file(), []Principal{{ID: 42, Login: "carol", Type: "User"}}, fixedNow)

	if !strings.HasPrefix(r.text, "::error::CLA v1 not signed by: carol\n") {
		t.Fatalf("the annotation must be the first line, got %q", r.text)
	}
	for _, want := range []string{`"login": "carol"`, `"id":    42`, `"date":  "2026-08-30"`, `"cla":   "v1"`} {
		if !strings.Contains(r.text, want) {
			t.Errorf("want %q in the log report", want)
		}
	}
	// The summary carries the file as it should be committed, so it is marshaled
	// rather than aligned by hand.
	for _, want := range []string{`"login": "carol"`, `"id": 42`, `"date": "2026-08-30"`, `"cla": "v1"`} {
		if !strings.Contains(r.markdown, want) {
			t.Errorf("want %q in the job summary", want)
		}
	}
	claURL := "https://github.com/pug-sh/pug/blob/main/CLA.md"
	if !strings.Contains(r.text, claURL) || !strings.Contains(r.markdown, claURL) {
		t.Errorf("both halves must link the agreement at %s", claURL)
	}
}

// The code block is meant to replace signatures/cla.json outright, so it has to
// parse as that whole file — not merely look like JSON.
func TestUnsignedReportEmitsPasteableJSON(t *testing.T) {
	r := unsignedReport(reportCfg(), file(), []Principal{{ID: 42, Login: "carol", Type: "User"}}, fixedNow)

	_, block, ok := strings.Cut(r.markdown, "```json\n")
	if !ok {
		t.Fatalf("want a json block in the summary, got %q", r.markdown)
	}
	block, _, ok = strings.Cut(block, "```")
	if !ok {
		t.Fatalf("want the json block to be closed, got %q", r.markdown)
	}

	var pasted SignatureFile
	if err := json.Unmarshal([]byte(block), &pasted); err != nil {
		t.Fatalf("the block is not a usable signatures/cla.json: %v\n%s", err, block)
	}
	entries := pasted.Signatures
	if len(entries) != 1 || entries[0].ID != 42 {
		t.Fatalf("want carol's entry with her id, got %+v", entries)
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

// A login other than the opener's can be invented by a Co-authored-by trailer, so
// mentioning one would let any pull request make the bot notify anyone it names.
// Only the opener, whom the webhook supplies, is mentioned.
func TestOnlyTheOpenerIsMentioned(t *testing.T) {
	cfg := reportCfg()
	cfg.opener = Principal{ID: 42, Login: "carol", Type: "User"}
	r := unsignedReport(cfg, file(), []Principal{
		{ID: 42, Login: "carol", Type: "User"},
		{ID: 7, Login: "torvalds", Type: "User"},
	}, fixedNow)

	if !strings.Contains(r.comment, "@carol") {
		t.Errorf("the comment must mention the opener, got %q", r.comment)
	}
	if strings.Contains(r.comment, "@torvalds") {
		t.Errorf("a co-author must not be mentioned, got %q", r.comment)
	}
	if strings.Contains(r.markdown, "@carol") {
		t.Errorf("the job summary must not mention, got %q", r.markdown)
	}
	if !strings.HasPrefix(r.comment, commentMarker) {
		t.Errorf("the comment must carry the marker so a re-run finds it, got %q", r.comment)
	}
	if !strings.HasPrefix(signedComment("v1"), commentMarker) {
		t.Error("the signed note must carry the marker too, or it posts as a second comment")
	}
}

// Pasting the block must not cost anyone their signature: the whole-file form
// reproduces the entries already there, which is what appendOnly demands back.
func TestWholeFileFormKeepsExistingSignatures(t *testing.T) {
	head := file(sig("alice", 1))
	head.Comment = "append-only"
	r := unsignedReport(reportCfg(), head, []Principal{{ID: 42, Login: "carol", Type: "User"}}, fixedNow)

	_, block, _ := strings.Cut(r.comment, "```json\n")
	block, _, _ = strings.Cut(block, "```")

	var pasted SignatureFile
	if err := json.Unmarshal([]byte(block), &pasted); err != nil {
		t.Fatalf("the block is not a usable signatures/cla.json: %v", err)
	}
	if pasted.Comment != head.Comment || pasted.CLAVersion != "v1" {
		t.Fatalf("want the file's own fields carried over, got %+v", pasted)
	}
	if len(pasted.Signatures) != 2 || pasted.Signatures[0] != head.Signatures[0] {
		t.Fatalf("want alice reproduced verbatim and carol appended, got %+v", pasted.Signatures)
	}
	// The one edit left to the contributor is their name; everything else the
	// gate will accept as it stands.
	if err := pasted.validate(); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("want the placeholder to be the only thing left to fix, got %v", err)
	}
	pasted.Signatures[1].Name = "Carol Danvers"
	if err := pasted.validate(); err != nil {
		t.Fatalf("the pasted file must pass once named: %v", err)
	}
	if err := appendOnly(head, &pasted, map[int64]bool{42: true}); err != nil {
		t.Fatalf("following the instructions must satisfy the gate: %v", err)
	}
}

// The two forms are told apart by what the block parses as, not by the prose
// above it: reword that sentence and a phrasing assertion passes vacuously.
func wholeFileForm(t *testing.T, comment string) bool {
	t.Helper()
	_, block, ok := strings.Cut(comment, "```json\n")
	if !ok {
		t.Fatalf("want a json block, got %q", comment)
	}
	block, _, _ = strings.Cut(block, "```")
	var f SignatureFile
	return json.Unmarshal([]byte(block), &f) == nil && f.CLAVersion != ""
}

// A file past the cap falls back to the entry alone: a comment reproducing every
// existing signature buries the one line the contributor has to add.
func TestLongSignatureFileFallsBackToTheEntryForm(t *testing.T) {
	long, atCap := file(), file()
	for i := range fullFileMaxSignatures + 1 {
		long.Signatures = append(long.Signatures, sig(fmt.Sprintf("dev%d", i), int64(i+100)))
		if i < fullFileMaxSignatures {
			atCap.Signatures = append(atCap.Signatures, sig(fmt.Sprintf("dev%d", i), int64(i+100)))
		}
	}
	carol := []Principal{{ID: 42, Login: "carol", Type: "User"}}

	r := unsignedReport(reportCfg(), long, carol, fixedNow)
	if wholeFileForm(t, r.comment) {
		t.Fatalf("want the entry form past the cap, got %q", r.comment)
	}
	if !strings.Contains(r.comment, `"id":    42`) {
		t.Fatalf("want carol's entry to append, got %q", r.comment)
	}
	// The cap is exclusive, so exactly fullFileMaxSignatures still fits.
	if !wholeFileForm(t, unsignedReport(reportCfg(), atCap, carol, fixedNow).comment) {
		t.Fatal("want the whole-file form at exactly the cap")
	}
}

// Two placeholders in one pasted file is a trap: whoever pastes it fills in their
// own name, and the other fails validate on the next run before any report runs.
func TestMultipleSignersFallBackToTheEntryForm(t *testing.T) {
	r := unsignedReport(reportCfg(), file(), []Principal{
		{ID: 42, Login: "carol", Type: "User"},
		{ID: 7, Login: "dave", Type: "User"},
	}, fixedNow)

	if wholeFileForm(t, r.comment) {
		t.Fatalf("want the entry form for two signers, got %q", r.comment)
	}
	if !strings.Contains(r.comment, "replacing each") {
		t.Fatalf("want the instruction to name every placeholder, got %q", r.comment)
	}
}
