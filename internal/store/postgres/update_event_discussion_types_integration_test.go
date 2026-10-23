package postgres

import (
	"context"
	"testing"

	"telesrv/internal/domain"
)

// TestUpdateEventAppendDiscussionTypes 回归迁移 0003：forum 话题已读的 durable 事件类型
// read_channel_discussion_inbox / read_channel_discussion_outbox 必须能写入 user_update_events。
// 0001 的 user_update_events_type_check 白名单原本遗漏这两种类型（0002/a180fbc 加了话题已读
// 功能却没补约束），导致真 PostgreSQL 上 messages.readDiscussion 首次标已读 append durable 事件
// 被 CHECK 约束拒（SQLSTATE 23514）→ RPC 500，且话题已读不进 durable / getDifference。
// memory store 无 CHECK 约束故单测未暴露，真双机 DrKLO 才发现。
func TestUpdateEventAppendDiscussionTypes(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	suffix := randomSuffix(t)

	users := NewUserStore(pool)
	u, err := users.Create(ctx, domain.User{AccessHash: 71, Phone: "+1675" + suffix + "01", FirstName: "DiscRead"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM users WHERE id = $1", u.ID) })

	store := NewUpdateEventStore(pool)
	const channelID = int64(990077)
	for _, et := range []domain.UpdateEventType{
		domain.UpdateEventReadChannelDiscussionInbox,
		domain.UpdateEventReadChannelDiscussionOutbox,
	} {
		ev, aerr := store.AppendAllocated(ctx, u.ID, domain.UpdateEvent{
			Type:     et,
			Peer:     domain.Peer{Type: domain.PeerTypeChannel, ID: channelID},
			TopMsgID: 51,
			MaxID:    100,
			PtsCount: 1,
			Date:     1700000900,
		})
		if aerr != nil {
			t.Fatalf("append %s 失败（迁移 0003 约束白名单是否缺该类型?）: %v", et, aerr)
		}
		if ev.Pts <= 0 {
			t.Fatalf("append %s: pts=%d, want >0", et, ev.Pts)
		}
	}

	events, err := store.ListAfter(ctx, u.ID, 0, 50)
	if err != nil {
		t.Fatalf("list after: %v", err)
	}
	seen := map[domain.UpdateEventType]bool{}
	for _, e := range events {
		seen[e.Type] = true
	}
	if !seen[domain.UpdateEventReadChannelDiscussionInbox] || !seen[domain.UpdateEventReadChannelDiscussionOutbox] {
		t.Fatalf("discussion 已读事件无法经 ListAfter 回放（多设备 getDifference 同步缺失）: seen=%v", seen)
	}
}
