package postgres

import (
	"context"

	"go.uber.org/zap"

	"telesrv/internal/store/postgres/sqlcgen"
)

func (s *MessageStore) reservePts(ctx context.Context, db sqlcgen.DBTX, userID int64) (int, error) {
	return s.reservePtsN(ctx, db, userID, 1)
}

func (s *MessageStore) reservePtsN(ctx context.Context, db sqlcgen.DBTX, userID int64, count int) (int, error) {
	count = normalizePtsCount(count)
	caller := traceCaller(2)
	pts, err := reserveUserPts(ctx, db, userID, count)
	if err != nil {
		s.log.Warn("pts_reserve_failed",
			zap.String("scope", "user"),
			zap.Int64("user_id", userID),
			zap.Int("pts_count", count),
			zap.String("caller", caller),
			zap.Error(err),
			zap.Error(ctx.Err()),
		)
		return 0, err
	}
	s.log.Debug("pts_reserve",
		zap.String("scope", "user"),
		zap.Int64("user_id", userID),
		zap.Int("pts", pts),
		zap.Int("pts_count", count),
		zap.String("caller", caller),
	)
	return pts, nil
}

type pgBoxIDAllocator struct {
	s *MessageStore
}
