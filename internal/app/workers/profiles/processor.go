package profiles

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	profilesv1 "github.com/fivebitsio/cotton/internal/gen/proto/profiles/v1"
	"github.com/fivebitsio/cotton/internal/gen/repo/dbwrite"
	"github.com/fivebitsio/cotton/internal/slogx"
)

type Worker struct {
	pgW   *pgxpool.Pool
	write *dbwrite.Queries
}

func NewWorker(pgW *pgxpool.Pool) *Worker {
	return &Worker{
		pgW:   pgW,
		write: dbwrite.New(pgW),
	}
}

func (w *Worker) ProcessMessage(ctx context.Context, data []byte) error {
	msg := &profilesv1.ProfileOperationMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		slog.ErrorContext(ctx, "failed to unmarshal profile operation message", slogx.Error(err))
		return err
	}

	switch msg.OperationType {
	case profilesv1.ProfileOperationType_PROFILE_OPERATION_TYPE_REGISTER:
		return w.handleRegister(ctx, msg)
	case profilesv1.ProfileOperationType_PROFILE_OPERATION_TYPE_IDENTIFY:
		return w.handleIdentify(ctx, msg)
	default:
		slog.WarnContext(ctx, "unknown profile operation type", slog.Int("type", int(msg.OperationType)))
		return fmt.Errorf("unknown operation type: %v", msg.OperationType)
	}
}
