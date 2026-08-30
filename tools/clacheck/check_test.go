package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func sig(login string, id int64) Signature {
	return Signature{Login: login, ID: id, Name: "N", Date: "2026-01-01", CLA: "v1"}
}

func file(s ...Signature) *SignatureFile {
	if s == nil {
		s = []Signature{}
	}
	return &SignatureFile{CLAVersion: "v1", Signatures: s}
}

func user(login string, id int64) *Principal { return &Principal{ID: id, Login: login, Type: "User"} }

func commit(sha string, author, committer *Principal, msg string) Commit {
	c := Commit{SHA: sha, Author: author, Committer: committer}
	c.Commit.Message = msg
	return c
}

// Each case takes a file that validates and breaks exactly one thing about it,
// so what is under test is the field named in the case, not a wall of JSON.
func TestValidateRejectsSignaturesThatRecordNothing(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*SignatureFile)
		want    string
	}{
		{"missing id", func(f *SignatureFile) { f.Signatures[0].ID = 0 }, "id is missing"},
		{"empty login", func(f *SignatureFile) { f.Signatures[0].Login = "" }, "login is empty"},
		{"empty name", func(f *SignatureFile) { f.Signatures[0].Name = "" }, "name is empty"},
		{"placeholder name", func(f *SignatureFile) { f.Signatures[0].Name = placeholderName }, "still the placeholder"},
		{"bad date", func(f *SignatureFile) { f.Signatures[0].Date = "yesterday" }, "date is not YYYY-MM-DD"},
		{"malformed cla", func(f *SignatureFile) { f.Signatures[0].CLA = "" }, "cla is missing or malformed"},
		{"no cla_version", func(f *SignatureFile) { f.CLAVersion = "" }, "cla_version is missing"},
		{"multiline cla_version", func(f *SignatureFile) { f.CLAVersion = "v1\n::error::injected" }, "must be one line"},
		{"signatures absent", func(f *SignatureFile) { f.Signatures = nil }, "signatures is missing"},
		{"same id twice at one version", func(f *SignatureFile) {
			f.Signatures = append(f.Signatures, sig("b", 1))
		}, "repeats the id and version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := file(sig("a", 1))
			if err := f.validate(); err != nil {
				t.Fatalf("the starting file must be valid: %v", err)
			}
			tt.corrupt(f)
			err := f.validate()
			if err == nil {
				t.Fatal("want an error, got none")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want error containing %q, got %q", tt.want, err)
			}
		})
	}
}

// The absent key must decode to nil rather than an empty array: that is the only
// thing separating "this file has no signatures key" from "the array is empty".
func TestAbsentSignaturesKeyDecodesToNil(t *testing.T) {
	var f SignatureFile
	if err := json.Unmarshal([]byte(`{"cla_version":"v1"}`), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.Signatures != nil {
		t.Fatalf("want nil, got %v", f.Signatures)
	}
	if err := f.validate(); err == nil || !strings.Contains(err.Error(), "signatures is missing") {
		t.Fatalf("want the missing-signatures error, got %v", err)
	}
}

// A signatures value that is not an array must fail to decode rather than being
// iterated as an object, which is how a jq-based check silently accepted it.
func TestSignaturesMustBeAnArray(t *testing.T) {
	var f SignatureFile
	if err := json.Unmarshal([]byte(`{"cla_version":"v1","signatures":{"a":{"id":1}}}`), &f); err == nil {
		t.Fatal("want a decode error for an object-shaped signatures")
	}
}

func TestUnsignedIdentifiesPrincipalsByID(t *testing.T) {
	head := file(sig("alice", 1))
	// A rename does not lose the signature, and re-registering the freed login
	// does not inherit it: identity is the id.
	missing, checked := unsigned(head, []Principal{{ID: 1, Login: "alice-renamed", Type: "User"}})
	if len(missing) != 0 || len(checked) != 1 {
		t.Fatalf("renamed signer should pass: missing=%v checked=%v", missing, checked)
	}
	missing, _ = unsigned(head, []Principal{{ID: 99, Login: "alice", Type: "User"}})
	if len(missing) != 1 {
		t.Fatalf("someone re-registering the login must not inherit the signature: %v", missing)
	}
}

// The bot exemption reads the account type from the API. A login merely shaped
// like a bot's is a person; a glob-based allowlist could be tricked into exempting
// one by adding a file named after it.
func TestOnlyRealBotsAreExempt(t *testing.T) {
	head := file()
	missing, _ := unsigned(head, []Principal{{ID: 1, Login: "dependabot[bot]", Type: "Bot"}})
	if len(missing) != 0 {
		t.Fatalf("a real bot needs no signature, got %v", missing)
	}
	for _, login := range []string{"dependabott", "renovateb", "github-actionso", "dependabot[bot]"} {
		missing, _ := unsigned(head, []Principal{{ID: 5, Login: login, Type: "User"}})
		if len(missing) != 1 {
			t.Fatalf("%q is a user account and must sign, got %v", login, missing)
		}
	}
}

func TestUnsignedDeduplicatesAndSkipsUnknownIDs(t *testing.T) {
	head := file(sig("alice", 1))
	people := []Principal{
		{ID: 1, Login: "alice", Type: "User"},
		{ID: 1, Login: "alice", Type: "User"},
		{ID: 2, Login: "bob", Type: "User"},
		{ID: 0, Login: "", Type: "User"},
	}
	missing, checked := unsigned(head, people)
	if len(checked) != 2 {
		t.Fatalf("want 2 distinct principals, got %v", checked)
	}
	if len(missing) != 1 || missing[0].Login != "bob" {
		t.Fatalf("want bob missing, got %v", missing)
	}
}

// Setting --author to someone who has signed does not launder the commit: the
// committer is the account that actually pushed it.
func TestCommitterIsCheckedNotJustAuthor(t *testing.T) {
	commits := []Commit{commit("a1", user("alice", 1), user("mallory", 2), "x")}
	people, unlinked := principals(commits, Principal{ID: 1, Login: "alice", Type: "User"})
	if len(unlinked) != 0 {
		t.Fatalf("nothing should be unlinked: %v", unlinked)
	}
	missing, _ := unsigned(file(sig("alice", 1)), people)
	if len(missing) != 1 || missing[0].Login != "mallory" {
		t.Fatalf("want the committer flagged, got %v", missing)
	}
}

// Excluded by id: the login comes from a commit email the contributor chooses,
// so anyone could have named themselves web-flow and dropped out of the check.
func TestWebFlowCommitterIsIgnored(t *testing.T) {
	commits := []Commit{commit("a1", user("alice", 1), user("web-flow", webFlowID), "x")}
	people, _ := principals(commits, Principal{ID: 1, Login: "alice", Type: "User"})
	missing, _ := unsigned(file(sig("alice", 1)), people)
	if len(missing) != 0 {
		t.Fatalf("GitHub's web-flow committer is not a copyright holder: %v", missing)
	}
	impostor := []Commit{commit("a1", user("alice", 1), user("web-flow", 5), "x")}
	people, _ = principals(impostor, Principal{ID: 1, Login: "alice", Type: "User"})
	if missing, _ := unsigned(file(sig("alice", 1)), people); len(missing) != 1 {
		t.Fatalf("only the real web-flow id is exempt, got %v", missing)
	}
}

// A commit whose email is not linked to an account must be reported, not dropped.
// jq's `//` applied to the whole stream, so a null author among linked ones vanished.
func TestUnlinkedCommitsAreReportedNotDropped(t *testing.T) {
	commits := []Commit{
		commit("a1", user("alice", 1), user("alice", 1), "x"),
		commit("b2", nil, nil, "y"),
		commit("c3", user("bob", 2), user("bob", 2), "z"),
	}
	_, unlinked := principals(commits, Principal{ID: 1, Login: "alice", Type: "User"})
	if len(unlinked) != 1 || unlinked[0] != "b2" {
		t.Fatalf("want b2 reported as unlinked, got %v", unlinked)
	}
}

func TestCoauthorTrailersAreCollected(t *testing.T) {
	msg := "feat: thing\n\nCo-authored-by: Bob <99+bob@users.noreply.github.com>\n" +
		"co-authored-by: Carol <carol@example.com>\n"
	got := coauthorEmails([]Commit{commit("a1", user("alice", 1), user("alice", 1), msg)})
	want := []string{"99+bob@users.noreply.github.com", "carol@example.com"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
}

// The id in a noreply address is commit-message text. Taking it on trust let a
// pull request sign for anyone whose id it wrote into a trailer, so only the
// login survives parsing and it is resolved against the API.
func TestNoreplyLoginDiscardsTheEmbeddedID(t *testing.T) {
	for email, want := range map[string]string{
		"99+bob@users.noreply.github.com":                   "bob",
		"bob@users.noreply.github.com":                      "bob",
		"49699333+dependabot[bot]@users.noreply.github.com": "dependabot[bot]",
		"bob@example.com":                                   "",
	} {
		if got := noreplyLogin(email); got != want {
			t.Errorf("noreplyLogin(%q) = %q, want %q", email, got, want)
		}
	}
}

// A version bump is signed by adding an entry, so the old one stays and both
// coexist; only an entry at the file's current version counts as signed.
func TestSignedRequiresTheCurrentVersion(t *testing.T) {
	f := &SignatureFile{CLAVersion: "v2", Signatures: []Signature{
		{Login: "alice", ID: 1, Name: "A", Date: "2026-01-01", CLA: "v1"},
		{Login: "bob", ID: 2, Name: "B", Date: "2026-01-01", CLA: "v2"},
	}}
	if err := f.validate(); err != nil {
		t.Fatalf("v1 and v2 entries must coexist: %v", err)
	}
	if f.signed(1) {
		t.Error("an entry left at v1 is not a v2 signature")
	}
	if !f.signed(2) {
		t.Error("a v2 entry is signed")
	}
}

// A trailer spanning a newline would inject its own ::workflow:: commands into
// the annotation stream when the address is echoed back.
func TestCoauthorTrailerStopsAtTheLineEnd(t *testing.T) {
	got := coauthorEmails([]Commit{commit("a1", user("alice", 1), user("alice", 1),
		"feat\n\nCo-authored-by: X <bob@example.com\n::error::injected>\n")})
	if len(got) != 0 {
		t.Fatalf("a trailer with a newline inside the address is not an address: %v", got)
	}
}

func TestAppendOnly(t *testing.T) {
	base := file(sig("alice", 1))
	mine := map[int64]bool{2: true}

	if err := appendOnly(base, file(sig("alice", 1), sig("bob", 2)), mine); err != nil {
		t.Fatalf("adding your own signature is the whole point: %v", err)
	}
	if err := appendOnly(base, file(sig("bob", 2)), mine); err == nil {
		t.Fatal("removing someone else's signature must be rejected")
	}
	edited := file(sig("alice", 1), sig("bob", 2))
	edited.Signatures[0].Name = "Someone Else"
	if err := appendOnly(base, edited, mine); err == nil {
		t.Fatal("editing someone else's signature must be rejected")
	}
	if err := appendOnly(base, file(sig("alice", 1), sig("stranger", 42)), mine); err == nil {
		t.Fatal("signing for someone who authored nothing here must be rejected")
	}
}
