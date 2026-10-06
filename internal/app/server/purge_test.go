package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestWarnStalledDeletions(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	for _, tc := range []struct {
		found bool
		err   error
		want  string
	}{
		{found: false},
		{found: true, want: "pug cron purge is not running"},
		{err: errors.New("db down"), want: "db down"},
	} {
		logs.Reset()
		warnStalledDeletions(t.Context(), func(context.Context) (bool, error) { return tc.found, tc.err })
		if got := logs.String(); (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("found %t, err %v: logs = %q, want %q", tc.found, tc.err, got, tc.want)
		}
	}
}
