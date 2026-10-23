package store

import (
	"context"

	"telesrv/internal/domain"
)

// ReadModelVersionStore exposes durable read-model version tokens to app services.
type ReadModelVersionStore interface {
	ReadModelHash(ctx context.Context, model string, ownerUserID int64, peerType domain.PeerType, peerID int64) (int64, bool, error)
	ReadModelHashes(ctx context.Context, keys []ReadModelKey) (map[ReadModelKey]int64, error)
}

type ReadModelVersionCache interface {
	InvalidateReadModel(ReadModelKey)
	FlushReadModelCache()
}

type ReadModelVersionCacheUpdater interface {
	UpdateReadModelHash(ReadModelKey, int64)
}

type ReadModelKey struct {
	Model       string
	OwnerUserID int64
	PeerType    domain.PeerType
	PeerID      int64
}
