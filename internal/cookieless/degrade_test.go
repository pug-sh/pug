package cookieless

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestDegradeReasons_AreExhaustivelyClassified pins the session-degrade
// vocabulary against silent ADDITION, the way TestDropReasons_RegistryMatchesDeclaredConstants
// (events handler) and TestExcludeCookielessForAgg_IsExhaustive (insights) pin
// theirs.
//
// Each DegradeReason drives a distinct operator response and ships to
// events.cookieless_session_degraded_total as its `reason` label. DegradeReason
// is a plain Go string type, not a proto enum, so there is no generated _name map
// to range over — this test reads the constant declarations straight out of
// session.go with go/ast and requires every one to carry an explicit decision in
// the table below. A new Degrade* constant added in code but not classified here
// fails the build, forcing the author to state the one thing the type cannot:
// does its presence mean SessionID returned the coarse visitor-day FALLBACK, or a
// usable/real id?
func TestDegradeReasons_AreExhaustivelyClassified(t *testing.T) {
	// coarseFallback[r] == true  → SessionID returned the deterministic
	//   one-session-per-visitor-day fallback (identity intact, sessions coarsen).
	// coarseFallback[r] == false → the returned id is real: fully stitched (None),
	//   correct-but-stale-watermark (SlideFailed), or freshly minted over a
	//   self-healing corrupt key (CorruptState).
	// This mirrors the get/mint/write-vs-slide split the ingest metric description
	// documents, so a new reason cannot ship without declaring which side it is on.
	coarseFallback := map[DegradeReason]bool{
		DegradeNone:         false,
		DegradeGetFailed:    true,
		DegradeMintFailed:   true,
		DegradeWriteFailed:  true,
		DegradeSlideFailed:  false,
		DegradeCorruptState: false,
	}

	declared := degradeReasonConstsFromSource(t)
	if len(declared) == 0 {
		t.Fatal("found no Degrade* DegradeReason constants in session.go — the AST scan broke, not the code")
	}

	byValue := map[string]string{} // value -> const name, for duplicate detection
	for name, value := range declared {
		if _, ok := coarseFallback[DegradeReason(value)]; !ok {
			t.Errorf("const %s = %q has no classification: add it to coarseFallback and decide — "+
				"does SessionID return the coarse visitor-day fallback for it (true) or a real id (false)?",
				name, value)
		}
		if value == "" && name != "DegradeNone" {
			t.Errorf("const %s is the empty string, reserved for DegradeNone (the not-degraded sentinel)", name)
		}
		if prior, dup := byValue[value]; dup {
			t.Errorf("duplicate DegradeReason value %q on %s and %s: the metric label and throttle-map key would collide",
				value, prior, name)
		}
		byValue[value] = name
	}

	// Reverse direction: the table must not drift ahead of the code either — every
	// classified reason has to be a constant that actually exists in session.go.
	for r := range coarseFallback {
		if _, ok := byValue[string(r)]; !ok {
			t.Errorf("coarseFallback classifies %q, which is not a declared Degrade* constant in session.go", r)
		}
	}
}

// TestCorruptOrNone_IsTotal pins the None/Corrupt selector — the one place the
// classification above is computed in production — as a total function, and pins
// DegradeNone as the empty sentinel the throttle map and the handler rely on.
func TestCorruptOrNone_IsTotal(t *testing.T) {
	if got := corruptOrNone(true); got != DegradeCorruptState {
		t.Errorf("corruptOrNone(true) = %q, want %q", got, DegradeCorruptState)
	}
	if got := corruptOrNone(false); got != DegradeNone {
		t.Errorf("corruptOrNone(false) = %q, want %q", got, DegradeNone)
	}
	if DegradeNone != "" {
		t.Errorf("DegradeNone must be the empty sentinel, got %q", DegradeNone)
	}
}

// degradeReasonConstsFromSource returns every `Name DegradeReason = "value"`
// constant declared in session.go, keyed by constant name.
func degradeReasonConstsFromSource(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "session.go", nil, 0)
	if err != nil {
		t.Fatalf("parse session.go: %v", err)
	}
	out := map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// Only DegradeReason-typed consts with a string-literal value.
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "DegradeReason" {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", name.Name, err)
				}
				out[name.Name] = v
			}
		}
	}
	return out
}
