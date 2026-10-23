package postgres

import (
	"context"
	"sync/atomic"
	"testing"

	"telesrv/internal/domain"
)

// staleChannelIDAllocator 模拟落后的 channel id 计数器：从 start 起按 1
// 步进，必然撞上已存在的 channel 主键。
type staleChannelIDAllocator struct {
	next    atomic.Int64
	atLeast atomic.Int64 // 记录 NextChannelIDAtLeast 被调用时的 floor
}

func (a *staleChannelIDAllocator) NextChannelID(_ context.Context) (int64, error) {
	return a.next.Add(1), nil
}

func (a *staleChannelIDAllocator) CurrentChannelID(_ context.Context) (int64, error) {
	return a.next.Load(), nil
}

func (a *staleChannelIDAllocator) NextChannelIDAtLeast(_ context.Context, floor int64) (int64, error) {
	a.atLeast.Store(floor)
	for {
		cur := a.next.Load()
		if cur < floor {
			if !a.next.CompareAndSwap(cur, floor) {
				continue
			}
		}
		return a.next.Add(1), nil
	}
}

// TestChannelStoreCreateChannelRecoversFromStaleIDCounter 验证计数器落后
// （Redis 快照回退 / 测试 fallback 分配器绕过 Redis 直写同一库）时，
// CreateChannel 经预检自愈拿到空闲 id，而非撞主键 500。
func TestChannelStoreCreateChannelRecoversFromStaleIDCounter(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	suffix := randomSuffix(t)

	users := NewUserStore(pool)
	creator, err := users.Create(ctx, domain.User{AccessHash: 91, Phone: "+1673" + suffix + "01", FirstName: "StaleIDCreator"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM users WHERE id = $1", creator.ID)
	})

	// 先用默认 PG fallback 分配器造一个真实 channel（占住 max id）。
	seedStore := NewChannelStore(pool)
	seed, err := seedStore.CreateChannel(ctx, domain.CreateChannelRequest{
		CreatorUserID: creator.ID,
		Title:         "StaleSeed " + suffix,
		Megagroup:     true,
		Date:          1700000700,
	})
	if err != nil {
		t.Fatalf("seed channel: %v", err)
	}

	// 落后计数器从 seed id 之前很远处开始（模拟回退），首次分配必撞。
	stale := &staleChannelIDAllocator{}
	stale.next.Store(seed.Channel.ID - 3)
	staleStore := NewChannelStore(pool, WithChannelAllocators(stale, nil))
	// msgIDs 传 nil 会被 fallback 覆盖；channel pts 由 PG 事务直接维护。
	created, err := staleStore.CreateChannel(ctx, domain.CreateChannelRequest{
		CreatorUserID: creator.ID,
		Title:         "StaleRecovered " + suffix,
		Megagroup:     true,
		Date:          1700000701,
	})
	if err != nil {
		t.Fatalf("create channel with stale counter: %v", err)
	}
	if created.Channel.ID <= seed.Channel.ID {
		t.Fatalf("recovered channel id = %d, want > seed id %d", created.Channel.ID, seed.Channel.ID)
	}
	if stale.atLeast.Load() < seed.Channel.ID {
		t.Fatalf("NextChannelIDAtLeast floor = %d, want >= seed id %d（应按表内最大 id 对账）", stale.atLeast.Load(), seed.Channel.ID)
	}
}
