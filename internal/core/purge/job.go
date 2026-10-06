package purge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	coreusage "github.com/pug-sh/pug/internal/core/usage"
	"github.com/pug-sh/pug/internal/deps/postgres"
	"github.com/pug-sh/pug/internal/deps/telemetry"
	"github.com/pug-sh/pug/internal/gen/repo/dbread"
	"github.com/pug-sh/pug/internal/gen/repo/dbwrite"
	"github.com/pug-sh/pug/internal/slogx"
)

const (
	statusPending  = "pending"
	statusDeleting = "deleting"
	statusDone     = "done"
)

const (
	// The API-key cache's TTL. Past it, nothing resolves the project's keys.
	hold       = time.Hour
	roundEvery = time.Hour
	// The NATS streams' max_age. Past it, no copy of the project's messages exists.
	watchFor    = 30 * 24 * time.Hour
	stuckAfter  = 24 * time.Hour
	batchRows   = 5000
	batchBudget = 2 * time.Minute
	maxErrorLen = 1024
)

type erasing struct {
	projectID string
	// ClickHouse tables with rows left or a delete running.
	waiting []string
	// The last round's start, or the request.
	since time.Time
}

// Pass moves every open deletion forward once. Every due deletion's ClickHouse
// deletes are queued before any Postgres batch runs, so ClickHouse can apply
// them in one rewrite. Problems the pass only notices are recorded without
// failing it.
func (s *Service) Pass(ctx context.Context, now time.Time) error {
	// Started before the ClickHouse step, so the pass fits its caller's timeout.
	until := time.Now().Add(batchBudget)
	open, err := s.read.ListOpenProjectDeletions(ctx, postgres.NewTimestamptz(now.Add(-watchFor)))
	if err != nil {
		slog.ErrorContext(ctx, "failed to list project deletions", slogx.Error(err))
		telemetry.RecordError(ctx, err)
		return err
	}

	var errs []error
	var batched []erasing
	for _, d := range open {
		e, ok, err := s.advance(ctx, d, now)
		if err != nil {
			s.store(ctx, d.ProjectID, err)
			errs = append(errs, err)
			continue
		}
		if ok {
			batched = append(batched, e)
		}
	}

	for _, e := range batched {
		if err := s.erasePostgres(ctx, e, now, until); err != nil {
			s.store(ctx, e.projectID, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// advance runs one deletion's ClickHouse step, and reports whether it is now
// deleting.
func (s *Service) advance(ctx context.Context, d dbread.ProjectDeletion, now time.Time) (erasing, bool, error) {
	switch d.Status {
	case statusPending:
		if now.Sub(d.RequestedAt.Time) < hold {
			return erasing{}, false, nil
		}
		if err := s.begin(ctx, d, now); err != nil {
			return erasing{}, false, err
		}
	case statusDone:
		state, err := s.Check(ctx, Filter{ProjectID: d.ProjectID})
		if err != nil {
			return erasing{}, false, err
		}
		if !anyRows(state) {
			return erasing{}, false, nil
		}
		if err := s.reopen(ctx, d.ProjectID); err != nil {
			return erasing{}, false, err
		}
	case statusDeleting:
	default:
		return erasing{}, false, s.failed(ctx, d.ProjectID, "advance", fmt.Errorf("unknown status %q", d.Status))
	}
	e, err := s.eraseClickHouse(ctx, d, now)
	if err != nil {
		return erasing{}, false, err
	}
	return e, true, nil
}

// begin stores the project's last usage count and marks it deleting, in one
// transaction. A reopened deletion goes back to deleting, so this runs once.
func (s *Service) begin(ctx context.Context, d dbread.ProjectDeletion, now time.Time) error {
	var usage []coreusage.DailyUsage
	// No org: deleted before deletions were recorded, and its usage went with it.
	if d.OrgID.Valid {
		start, _, err := s.usage.GetOrgPeriod(ctx, d.OrgID.String, now)
		if err != nil {
			return err
		}
		from := coreusage.FullRecomputeFrom(now, coreusage.DefaultRescanDays, []coreusage.OrgPeriod{{Start: start}})
		if usage, err = s.usage.MeterProject(ctx, d.ProjectID, from, now); err != nil {
			return err
		}
	}

	return s.inTx(ctx, d.ProjectID, "start", func(w *dbwrite.Queries) error {
		n, err := w.StartProjectDeletion(ctx, d.ProjectID)
		if err != nil {
			return s.failed(ctx, d.ProjectID, "mark deleting", err)
		}
		// Another pass started it, and its deletes may already have run.
		if n == 0 {
			return nil
		}
		return coreusage.FreezeDailyUsageInTx(ctx, w, d.OrgID.String, d.ProjectID, usage)
	})
}

// eraseClickHouse queues the project's ClickHouse deletes. A new round starts at
// once, then at most once an hour while rows keep coming back.
func (s *Service) eraseClickHouse(ctx context.Context, d dbread.ProjectDeletion, now time.Time) (erasing, error) {
	f := Filter{ProjectID: d.ProjectID}
	state, err := s.Check(ctx, f)
	if err != nil {
		return erasing{}, err
	}
	e := erasing{projectID: d.ProjectID, since: d.RequestedAt.Time}
	if d.RoundStartedAt.Valid {
		e.since = d.RoundStartedAt.Time
	}
	leftover := false
	for _, t := range state {
		if t.FailReason != "" {
			s.finding(ctx, d.ProjectID, fmt.Errorf("purge: delete on %s is failing: %s", t.Name, t.FailReason))
		}
		if t.HasRows || t.Deleting {
			e.waiting = append(e.waiting, t.Name)
		}
		if t.HasRows && !t.Deleting && !t.Rebuilt {
			leftover = true
		}
	}
	if !leftover || (d.Rounds > 0 && now.Sub(d.RoundStartedAt.Time) < roundEvery) {
		return e, nil
	}

	queued, err := s.Start(ctx, f)
	if err != nil || len(queued) == 0 {
		return e, err
	}
	if err := s.write.RecordProjectDeletionRound(ctx, dbwrite.RecordProjectDeletionRoundParams{
		ProjectID:      d.ProjectID,
		RoundStartedAt: postgres.NewTimestamptz(now),
	}); err != nil {
		return erasing{}, s.failed(ctx, d.ProjectID, "record a round", err)
	}
	e.since = now
	if d.Rounds > 0 || d.Status == statusDone {
		s.finding(ctx, d.ProjectID, fmt.Errorf("purge: rows came back in %s; round %d started",
			strings.Join(queued, ", "), d.Rounds+1))
	}
	return e, nil
}

// erasePostgres deletes the project's rows in batches. Once they and ClickHouse
// are empty, it deletes the projects row and marks the deletion done.
func (s *Service) erasePostgres(ctx context.Context, e erasing, now, until time.Time) error {
	if err := s.refuseLive(ctx, Filter{ProjectID: e.projectID}); err != nil {
		return err
	}
	empty, err := s.deleteBatches(ctx, e.projectID, until)
	if err != nil {
		return err
	}
	waiting := e.waiting
	if !empty {
		waiting = append(waiting, "postgres")
	}
	if len(waiting) > 0 {
		if now.Sub(e.since) >= stuckAfter {
			s.finding(ctx, e.projectID, fmt.Errorf("purge: deletion not done %s after its last round started; waiting on %s",
				now.Sub(e.since).Round(time.Minute), strings.Join(waiting, ", ")))
		}
		return nil
	}
	err = s.inTx(ctx, e.projectID, "finish", func(w *dbwrite.Queries) error {
		if _, err := w.DeleteHiddenProject(ctx, e.projectID); err != nil {
			return s.failed(ctx, e.projectID, "delete the projects row", err)
		}
		if _, err := w.FinishProjectDeletion(ctx, dbwrite.FinishProjectDeletionParams{
			DoneAt:    postgres.NewTimestamptz(now),
			ProjectID: e.projectID,
		}); err != nil {
			return s.failed(ctx, e.projectID, "mark done", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "project deletion done", slog.String("project_id", e.projectID))
	return nil
}

// deleteBatches deletes devices before profiles: their profile_id is on delete
// set null, so a profile deleted first would rewrite its devices.
func (s *Service) deleteBatches(ctx context.Context, projectID string, until time.Time) (bool, error) {
	// Bounds a statement stuck on a row lock too.
	batchCtx, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	batches := []struct {
		table string
		run   func() (int64, error)
	}{
		{"profile_devices", func() (int64, error) {
			return s.write.DeleteProfileDevicesBatch(batchCtx, dbwrite.DeleteProfileDevicesBatchParams{
				ProjectID: projectID, RowLimit: batchRows,
			})
		}},
		{"profiles", func() (int64, error) {
			return s.write.DeleteProfilesBatch(batchCtx, dbwrite.DeleteProfilesBatchParams{
				ProjectID: projectID, RowLimit: batchRows,
			})
		}},
	}
	for _, b := range batches {
		for {
			if time.Now().After(until) {
				return false, nil
			}
			n, err := b.run()
			if err != nil {
				if batchCtx.Err() != nil && ctx.Err() == nil {
					return false, nil
				}
				return false, s.failed(ctx, projectID, "delete a batch of "+b.table, err)
			}
			if n < batchRows {
				break
			}
		}
	}
	return true, nil
}

func (s *Service) reopen(ctx context.Context, projectID string) error {
	if _, err := s.write.ReopenProjectDeletion(ctx, projectID); err != nil {
		return s.failed(ctx, projectID, "reopen", err)
	}
	slog.WarnContext(ctx, "rows came back after a project deletion finished; reopening",
		slog.String("project_id", projectID))
	return nil
}

// inTx runs fn in a transaction. fn reports its own errors.
func (s *Service) inTx(ctx context.Context, projectID, op string, fn func(*dbwrite.Queries) error) error {
	tx, err := s.pgW.Begin(ctx)
	if err != nil {
		return s.failed(ctx, projectID, "begin "+op, err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			slog.ErrorContext(ctx, "failed to roll back a project deletion step", slogx.Error(err),
				slog.String("project_id", projectID), slog.String("op", op))
			telemetry.RecordError(ctx, err)
		}
	}()
	if err := fn(s.write.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return s.failed(ctx, projectID, "commit "+op, err)
	}
	return nil
}

func anyRows(state []Table) bool {
	for _, t := range state {
		if t.HasRows {
			return true
		}
	}
	return false
}

func (s *Service) failed(ctx context.Context, projectID, op string, err error) error {
	err = fmt.Errorf("purge: %s: %w", op, err)
	slog.ErrorContext(ctx, "project deletion step failed", slogx.Error(err), slog.String("project_id", projectID))
	telemetry.RecordError(ctx, err)
	return err
}

// finding records a problem the pass noticed without failing it.
func (s *Service) finding(ctx context.Context, projectID string, err error) {
	slog.ErrorContext(ctx, "project deletion needs attention", slogx.Error(err), slog.String("project_id", projectID))
	telemetry.RecordError(ctx, err)
	s.store(ctx, projectID, err)
}

// store keeps the last problem on the deletion's row, for operators.
func (s *Service) store(ctx context.Context, projectID string, err error) {
	msg := err.Error()
	if len(msg) > maxErrorLen {
		msg = strings.ToValidUTF8(msg[:maxErrorLen], "")
	}
	if werr := s.write.SetProjectDeletionError(ctx, dbwrite.SetProjectDeletionErrorParams{
		Error:     msg,
		ProjectID: projectID,
	}); werr != nil {
		slog.ErrorContext(ctx, "failed to store a project deletion's error", slogx.Error(werr),
			slog.String("project_id", projectID))
		telemetry.RecordError(ctx, werr)
	}
}
