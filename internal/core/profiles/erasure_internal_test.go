package profiles

import "testing"

func TestChInClause(t *testing.T) {
	clause, args := chInClause([]string{"a", "b", "c"})
	if clause != "(?, ?, ?)" {
		t.Errorf("clause = %q, want (?, ?, ?)", clause)
	}
	if len(args) != 3 {
		t.Fatalf("args len = %d, want 3", len(args))
	}
	if args[0] != "a" || args[1] != "b" || args[2] != "c" {
		t.Errorf("args = %v, want [a b c]", args)
	}

	clause, args = chInClause(nil)
	if clause != "()" {
		t.Errorf("empty clause = %q, want ()", clause)
	}
	if len(args) != 0 {
		t.Errorf("empty args len = %d, want 0", len(args))
	}
}
