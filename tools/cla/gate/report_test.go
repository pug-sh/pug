package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// carol opens every report test: the entry the report hands out is the opener's,
// because appendOnly will not accept anyone else's.
func reportCfg() config {
	return config{repo: "pug-sh/pug", baseRef: "main", serverURL: "https://github.com",
		opener: Principal{ID: 42, Login: "carol", Type: "User"}}
}

func TestUnsignedReportFillsInTheContributorsEntry(t *testing.T) {
	r := unsignedReport(reportCfg(), file(), []Principal{{ID: 42, Login: "carol", Type: "User"}}, nil, fixedNow)

	if !strings.HasPrefix(r.text, "::error::CLA v1 not signed by: carol\n") {
		t.Fatalf("the annotation must be the first line, got %q", r.text)
	}
	for _, want := range []string{`"login": "carol"`, `"id": 42`, `"date": "2026-08-30"`, `"cla": "v1"`} {
		if !strings.Contains(r.text, want) {
			t.Errorf("want %q in the log report", want)
		}
	}
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

// The block is one entry for the contributor to append, so it has to parse as a
// signature — and appending it must be the whole of what the gate wants back.
func TestUnsignedReportEmitsAnAppendableEntry(t *testing.T) {
	head := file(sig("alice", 1))
	r := unsignedReport(reportCfg(), head, []Principal{{ID: 42, Login: "carol", Type: "User"}}, nil, fixedNow)

	_, block, ok := strings.Cut(r.comment, "```json\n")
	if !ok {
		t.Fatalf("want a json block in the comment, got %q", r.comment)
	}
	block, _, ok = strings.Cut(block, "```")
	if !ok {
		t.Fatalf("want the json block to be closed, got %q", r.comment)
	}

	var entry Signature
	if err := json.Unmarshal([]byte(block), &entry); err != nil {
		t.Fatalf("the block is not a usable signatures entry: %v\n%s", err, block)
	}
	if entry.ID != 42 || entry.Login != "carol" {
		t.Fatalf("want carol's entry with her id, got %+v", entry)
	}
	if entry.CLA != "v1" || entry.Date != "2026-08-30" {
		t.Fatalf("want the version in force and the current date, got %+v", entry)
	}

	// Appending it and changing nothing else is the whole of the instruction, so
	// it has to be the whole of what the gate asks for.
	signed := file(append(slices.Clone(head.Signatures), entry)...)
	if err := signed.validate(); err != nil {
		t.Fatalf("the appended file must pass as it stands: %v", err)
	}
	if err := appendOnly(head, signed, reportCfg().opener, "v1"); err != nil {
		t.Fatalf("following the instructions must satisfy the gate: %v", err)
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
	}, nil, fixedNow)

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

// A co-author is named but handed no entry: appendOnly would reject one added
// here, so printing it would be advice that fails on the next run.
func TestACoauthorIsNamedButGetsNoEntry(t *testing.T) {
	r := unsignedReport(reportCfg(), file(), []Principal{
		{ID: 42, Login: "carol", Type: "User"},
		{ID: 7, Login: "dave", Type: "User"},
	}, nil, fixedNow)

	for _, part := range []string{r.text, r.comment, r.markdown} {
		if !strings.Contains(part, "dave") {
			t.Errorf("the co-author must be named, got %q", part)
		}
		if strings.Contains(part, `"id": 7`) {
			t.Errorf("the co-author must not be handed an entry to paste, got %q", part)
		}
		if !strings.Contains(part, "opened themselves") {
			t.Errorf("want the way out named, got %q", part)
		}
	}
}

// The opener may already have signed, leaving only a co-author outstanding. There
// is then nothing for this pull request to add, and the report must not imply
// otherwise.
func TestOnlyACoauthorOutstandingOffersNoEntry(t *testing.T) {
	r := unsignedReport(reportCfg(), file(), []Principal{{ID: 7, Login: "dave", Type: "User"}}, nil, fixedNow)

	if strings.Contains(r.comment, "```json") {
		t.Errorf("want no entry offered when the opener has signed, got %q", r.comment)
	}
	if !strings.Contains(r.comment, "dave") || !strings.Contains(r.comment, "opened themselves") {
		t.Errorf("want dave named with the way out, got %q", r.comment)
	}
	if strings.Contains(r.comment, "\n\n\n") {
		t.Errorf("want no doubled blank line, got %q", r.comment)
	}
}

// A trailer address is commit-message text the pull request chose. It reaches an
// annotation, a log line and a comment, and must be inert in all three.
func TestHostileTrailerAddressIsNeutralised(t *testing.T) {
	hostile := []string{"a%0A::error::forged@x", "[click](https://evil.example)", "back`tick@x"}
	r := unsignedReport(reportCfg(), file(), []Principal{{ID: 42, Login: "carol", Type: "User"}}, hostile, fixedNow)

	lines := strings.FieldsFunc(r.text, func(r rune) bool { return r == '\n' || r == '\r' })
	if !strings.HasPrefix(lines[0], "::error::") {
		t.Fatalf("want the annotation first, got %q", lines[0])
	}
	if strings.Contains(lines[0], "%0A::error::") {
		t.Errorf("a raw %%0A would decode into a second workflow command: %q", lines[0])
	}
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "::") {
			t.Errorf("a log line opening with :: is a workflow command: %q", l)
		}
	}

	// Markdown gets it inside a code span, so a link renders as its own text.
	for _, want := range []string{"`[click](https://evil.example)`", "`backtick@x`"} {
		if !strings.Contains(r.comment, want) {
			t.Errorf("want %q in the comment, got %q", want, r.comment)
		}
	}
	if strings.Contains(r.comment, "back`tick") {
		t.Errorf("a backtick would close the span early, got %q", r.comment)
	}
}

// The opener is who the comment notifies. With nothing left for them to sign, a
// heading demanding a signature reads as a demand they have already met.
func TestHeadingFollowsWhatIsBlocking(t *testing.T) {
	carol := Principal{ID: 42, Login: "carol", Type: "User"}
	dave := Principal{ID: 7, Login: "dave", Type: "User"}

	mine := unsignedReport(reportCfg(), file(), []Principal{carol}, nil, fixedNow).comment
	if !strings.HasPrefix(mine, commentMarker+"\n## CLA signature required") {
		t.Errorf("want the signature heading when the opener owes one, got %q", mine)
	}

	theirs := unsignedReport(reportCfg(), file(), []Principal{dave}, nil, fixedNow).comment
	if !strings.HasPrefix(theirs, commentMarker+"\n## CLA check blocked") {
		t.Errorf("want the blocked heading when the opener owes nothing, got %q", theirs)
	}
	if strings.Contains(theirs, "signature required") {
		t.Errorf("a signed opener must not be asked to sign, got %q", theirs)
	}
	if !strings.Contains(theirs, "Still outstanding:") {
		t.Errorf("want the blocks introduced, got %q", theirs)
	}
}

// A comma-joined list reads as one subject taking a singular verb, so the count
// has to reach the verb and the noun as well as the join.
func TestBlocksAgreeInNumber(t *testing.T) {
	one := Principal{ID: 7, Login: "dave", Type: "User"}
	two := Principal{ID: 8, Login: "erin", Type: "User"}

	single := unsignedReport(reportCfg(), file(), []Principal{one}, []string{"a@x"}, fixedNow).comment
	for _, want := range []string{"dave has work", "A Co-authored-by trailer names"} {
		if !strings.Contains(single, want) {
			t.Errorf("want %q, got %q", want, single)
		}
	}

	plural := unsignedReport(reportCfg(), file(), []Principal{one, two}, []string{"a@x", "b@y"}, fixedNow).comment
	for _, want := range []string{"dave and erin have work", "Co-authored-by trailers name", "`a@x` and `b@y`"} {
		if !strings.Contains(plural, want) {
			t.Errorf("want %q, got %q", want, plural)
		}
	}

	if got := joinNames([]string{"a", "b", "c"}); got != "a, b and c" {
		t.Errorf("joinNames of three = %q", got)
	}
}
