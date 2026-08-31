package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func sig(login string, id int64) Signature {
	return Signature{Login: login, ID: id, Date: "2026-01-01", CLA: "v1"}
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
		{"bad date", func(f *SignatureFile) { f.Signatures[0].Date = "yesterday" }, "date is not a real"},
		{"impossible day", func(f *SignatureFile) { f.Signatures[0].Date = "2026-02-30" }, "date is not a real"},
		{"impossible month", func(f *SignatureFile) { f.Signatures[0].Date = "2026-99-99" }, "date is not a real"},
		{"unpadded date", func(f *SignatureFile) { f.Signatures[0].Date = "2026-2-3" }, "date is not a real"},
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
	missing, checked := unsigned(head, &SignatureFile{}, []Principal{{ID: 1, Login: "alice-renamed", Type: "User"}})
	if len(missing) != 0 || len(checked) != 1 {
		t.Fatalf("renamed signer should pass: missing=%v checked=%v", missing, checked)
	}
	missing, _ = unsigned(head, &SignatureFile{}, []Principal{{ID: 99, Login: "alice", Type: "User"}})
	if len(missing) != 1 {
		t.Fatalf("someone re-registering the login must not inherit the signature: %v", missing)
	}
}

// The bot exemption reads the account type from the API. A login merely shaped
// like a bot's is a person; a glob-based allowlist could be tricked into exempting
// one by adding a file named after it.
func TestOnlyRealBotsAreExempt(t *testing.T) {
	head := file()
	missing, _ := unsigned(head, &SignatureFile{}, []Principal{{ID: 1, Login: "dependabot[bot]", Type: "Bot"}})
	if len(missing) != 0 {
		t.Fatalf("a real bot needs no signature, got %v", missing)
	}
	for _, login := range []string{"dependabott", "renovateb", "github-actionso", "dependabot[bot]"} {
		missing, _ := unsigned(head, &SignatureFile{}, []Principal{{ID: 5, Login: login, Type: "User"}})
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
	missing, checked := unsigned(head, &SignatureFile{}, people)
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
	missing, _ := unsigned(file(sig("alice", 1)), &SignatureFile{}, people)
	if len(missing) != 1 || missing[0].Login != "mallory" {
		t.Fatalf("want the committer flagged, got %v", missing)
	}
}

// Excluded by id: the login comes from a commit email the contributor chooses,
// so anyone could have named themselves web-flow and dropped out of the check.
func TestWebFlowCommitterIsIgnored(t *testing.T) {
	commits := []Commit{commit("a1", user("alice", 1), user("web-flow", webFlowID), "x")}
	people, _ := principals(commits, Principal{ID: 1, Login: "alice", Type: "User"})
	missing, _ := unsigned(file(sig("alice", 1)), &SignatureFile{}, people)
	if len(missing) != 0 {
		t.Fatalf("GitHub's web-flow committer is not a copyright holder: %v", missing)
	}
	impostor := []Commit{commit("a1", user("alice", 1), user("web-flow", 5), "x")}
	people, _ = principals(impostor, Principal{ID: 1, Login: "alice", Type: "User"})
	if missing, _ := unsigned(file(sig("alice", 1)), &SignatureFile{}, people); len(missing) != 1 {
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
		{Login: "alice", ID: 1, Date: "2026-01-01", CLA: "v1"},
		{Login: "bob", ID: 2, Date: "2026-01-01", CLA: "v2"},
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

// A version bump retires the old text. An entry added against it would record an
// agreement never given, into a file that is append-only and is the record.
func TestANewSignatureMustBeAtTheVersionInForce(t *testing.T) {
	v1 := Signature{Login: "alice", ID: 1, Date: "2025-01-01", CLA: "v1"}
	v2 := func(s ...Signature) *SignatureFile { return &SignatureFile{CLAVersion: "v2", Signatures: s} }
	base := v2(v1)
	bob := Principal{ID: 2, Login: "bob", Type: "User"}

	stale := v2(v1, Signature{Login: "bob", ID: 2, Date: "2026-08-31", CLA: "v1"})
	err := appendOnly(base, stale, bob, "v2")
	if err == nil {
		t.Fatal("a new signature against the retired version must be rejected")
	}
	if !strings.Contains(err.Error(), "version in force") {
		t.Errorf("want the current version named, got %v", err)
	}
	// The bump does not invalidate what came before: alice's v1 entry is the
	// record of what she agreed to then, and stays as it is.
	current := v2(v1, Signature{Login: "bob", ID: 2, Date: "2026-08-31", CLA: "v2"})
	if err := appendOnly(base, current, bob, "v2"); err != nil {
		t.Fatalf("signing the version in force must be accepted: %v", err)
	}
	if err := current.validate(); err != nil {
		t.Fatalf("a v1 entry must survive the bump alongside a v2 one: %v", err)
	}
}

func TestAppendOnly(t *testing.T) {
	base := file(sig("alice", 1))
	bob := Principal{ID: 2, Login: "bob", Type: "User"}

	if err := appendOnly(base, file(sig("alice", 1), sig("bob", 2)), bob, "v1"); err != nil {
		t.Fatalf("adding your own signature is the whole point: %v", err)
	}
	if err := appendOnly(base, file(sig("bob", 2)), bob, "v1"); err == nil {
		t.Fatal("removing someone else's signature must be rejected")
	}
	edited := file(sig("alice", 1), sig("bob", 2))
	edited.Signatures[0].Date = "2020-01-01"
	if err := appendOnly(base, edited, bob, edited.CLAVersion); err == nil {
		t.Fatal("editing someone else's signature must be rejected")
	}
	if err := appendOnly(base, file(sig("alice", 1), sig("stranger", 42)), bob, "v1"); err == nil {
		t.Fatal("signing for anyone but the opener must be rejected")
	}
	// GitHub folds case in a login, and the report hands out the canonical form,
	// so a difference in case is a typo rather than a different person.
	if err := appendOnly(base, file(sig("alice", 1), sig("BoB", 2)), bob, "v1"); err != nil {
		t.Fatalf("a login differing only in case is the same person: %v", err)
	}
}

// The id is what signing is matched on, so an entry pairing the opener's id with
// somebody else's login stood in the file as a signature by that other person.
func TestSignatureLoginMustMatchTheIDItClaims(t *testing.T) {
	base := file()
	bob := Principal{ID: 2, Login: "bob", Type: "User"}

	err := appendOnly(base, file(sig("torvalds", 2)), bob, "v1")
	if err == nil || !strings.Contains(err.Error(), `belongs to "bob"`) {
		t.Fatalf("want the mismatched login rejected, got %v", err)
	}
}

// Only the noreply form is ever looked up, so an ordinary address is unidentified
// rather than absent from GitHub — the report must not claim a search it skipped.
func TestOnlyTheNoreplyFormYieldsALogin(t *testing.T) {
	for _, addr := range []string{"someone@example.com", "woof@pug.sh", "1+alice@users.noreply.github.example"} {
		if got := noreplyLogin(addr); got != "" {
			t.Errorf("%q is not the noreply form, got login %q", addr, got)
		}
	}
	// The id in the <id>+<login> form comes out of a commit message, so the login
	// is all that is taken from it.
	for addr, want := range map[string]string{
		"alice@users.noreply.github.com":             "alice",
		"12345+alice@users.noreply.github.com":       "alice",
		"1+dependabot[bot]@users.noreply.github.com": "dependabot[bot]",
	} {
		if got := noreplyLogin(addr); got != want {
			t.Errorf("noreplyLogin(%q) = %q, want %q", addr, got, want)
		}
	}
}

// A lone CR ends a line for the Actions log and, per CommonMark, for the comment
// too — so a trailer carrying one could forge a workflow command and break out of
// the code span the report renders it in.
func TestTrailerAddressCannotCarryACarriageReturn(t *testing.T) {
	got := coauthorEmails([]Commit{commit("a1", nil, nil,
		"feat\n\nCo-authored-by: M <a\r\r[click](https://evil.example)@x>\n")})
	if len(got) != 1 {
		t.Fatalf("want the trailer captured, got %v", got)
	}
	if strings.ContainsAny(got[0], "\r\n") {
		t.Fatalf("want no line ending in the address, got %q", got[0])
	}
}

// The version in force is the base branch's, not the merge base's: a branch cut
// before a bump would otherwise sign the retired version and pass.
func TestABranchBehindAVersionBumpCannotSignTheRetiredOne(t *testing.T) {
	mergeBase := file(sig("alice", 1))
	head := file(sig("alice", 1), sig("bob", 2))
	bob := Principal{ID: 2, Login: "bob", Type: "User"}

	err := appendOnly(mergeBase, head, bob, "v2")
	if err == nil || !strings.Contains(err.Error(), "merge the base branch") {
		t.Fatalf("want the stale version rejected with the way out, got %v", err)
	}
}

// A /sign comment records the signature on the base branch, so a contributor's
// first pull request predates their own signature. head alone would read them as
// unsigned however many times they signed.
func TestUnsignedAcceptsASignatureOnTheBaseBranch(t *testing.T) {
	head := &SignatureFile{CLAVersion: "v1"}
	onBase := &SignatureFile{CLAVersion: "v1", Signatures: []Signature{
		{Login: "nullorm", ID: 78271873, Date: "2026-09-01", CLA: "v1"},
	}}
	people := []Principal{{ID: 78271873, Login: "nullorm", Type: "User"}}

	missing, checked := unsigned(head, onBase, people)
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none: the signature is on the base branch", missing)
	}
	if len(checked) != 1 {
		t.Errorf("checked = %d, want 1", len(checked))
	}
}

// The base branch is searched at the version head declares, so a retired
// signature does not carry into a bumped version.
func TestUnsignedIgnoresABaseSignatureAtAnotherVersion(t *testing.T) {
	head := &SignatureFile{CLAVersion: "v2"}
	onBase := &SignatureFile{CLAVersion: "v2", Signatures: []Signature{
		{Login: "nullorm", ID: 78271873, Date: "2026-09-01", CLA: "v1"},
	}}
	people := []Principal{{ID: 78271873, Login: "nullorm", Type: "User"}}

	missing, _ := unsigned(head, onBase, people)
	if len(missing) != 1 {
		t.Errorf("missing = %v, want nullorm: v1 does not carry over to v2", missing)
	}
}

// The pull request's own file still counts, which is the hand-edited path.
func TestUnsignedStillAcceptsASignatureInHead(t *testing.T) {
	head := &SignatureFile{CLAVersion: "v1", Signatures: []Signature{
		{Login: "nullorm", ID: 78271873, Date: "2026-09-01", CLA: "v1"},
	}}
	onBase := &SignatureFile{CLAVersion: "v1"}
	people := []Principal{{ID: 78271873, Login: "nullorm", Type: "User"}}

	if missing, _ := unsigned(head, onBase, people); len(missing) != 0 {
		t.Errorf("missing = %v, want none: the signature is in head", missing)
	}
}
