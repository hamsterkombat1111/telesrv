package postgres

import (
	"context"
	"reflect"
	"testing"

	"telesrv/internal/domain"
)

// TestChannelPollFanoutViewsPostgres 验证 Phase 4 poll 模板化的 postgres 路径：新批量 SQL
// （batchPollViewerOptions / batchChannelMemberAvailableMinID + pollViewerAggregates(viewer=0)）
// 正确运行，且 ChannelPollFanoutViews 对每个 viewer 与逐 viewer GetChannelMessages（旧 fan-out 路径）
// 的 enrich poll 字节等价、可见性一致。门控于 TELESRV_TEST_POSTGRES_DSN。
func TestChannelPollFanoutViewsPostgres(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	suffix := randomSuffix(t)
	users := NewUserStore(pool)

	owner, err := users.Create(ctx, domain.User{AccessHash: 71, Phone: "+1893" + suffix + "01", FirstName: "PollOwner"})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	voted, err := users.Create(ctx, domain.User{AccessHash: 72, Phone: "+1893" + suffix + "02", FirstName: "PollVoted"})
	if err != nil {
		t.Fatalf("create voted: %v", err)
	}
	unvoted, err := users.Create(ctx, domain.User{AccessHash: 73, Phone: "+1893" + suffix + "03", FirstName: "PollUnvoted"})
	if err != nil {
		t.Fatalf("create unvoted: %v", err)
	}
	stranger, err := users.Create(ctx, domain.User{AccessHash: 74, Phone: "+1893" + suffix + "04", FirstName: "PollStranger"})
	if err != nil {
		t.Fatalf("create stranger: %v", err)
	}

	channels := NewChannelStore(pool)
	var channelID int64
	t.Cleanup(func() {
		if channelID != 0 {
			_, _ = pool.Exec(ctx, "DELETE FROM channels WHERE id = $1", channelID)
		}
		_, _ = pool.Exec(ctx, "DELETE FROM users WHERE id = ANY($1::bigint[])", []int64{owner.ID, voted.ID, unvoted.ID, stranger.ID})
	})
	created, err := channels.CreateChannel(ctx, domain.CreateChannelRequest{
		CreatorUserID: owner.ID,
		Title:         "Poll Fanout " + suffix,
		Megagroup:     true,
		MemberUserIDs: []int64{voted.ID, unvoted.ID},
		Date:          1700002000,
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	channelID = created.Channel.ID

	pollID := int64(9300000000) + (channelID % 1000)
	opt1, opt2 := []byte{1}, []byte{2}
	polls := NewPollStore(pool)
	if err := polls.CreatePoll(ctx, domain.PollDefinition{
		ID:            pollID,
		CreatorUserID: owner.ID,
		Options:       [][]byte{opt1, opt2},
		PublicVoters:  true,
	}); err != nil {
		t.Fatalf("create poll: %v", err)
	}
	sent, err := channels.SendChannelMessage(ctx, domain.SendChannelMessageRequest{
		UserID:    owner.ID,
		ChannelID: channelID,
		RandomID:  93_001,
		Date:      1700002001,
		Media: &domain.MessageMedia{
			Kind: domain.MessageMediaKindPoll,
			Poll: &domain.MessagePoll{
				ID:           pollID,
				Question:     "Q?",
				Answers:      []domain.MessagePollAnswer{{Text: "A", Option: opt1}, {Text: "B", Option: opt2}},
				PublicVoters: true,
			},
		},
	})
	if err != nil {
		t.Fatalf("send poll message: %v", err)
	}
	msgID := sent.Message.ID
	if _, err := channels.VoteChannelMessagePoll(ctx, domain.VoteChannelMessagePollRequest{
		UserID: voted.ID, ChannelID: channelID, MessageID: msgID, Options: [][]byte{opt1}, Date: 1700002002,
	}); err != nil {
		t.Fatalf("vote: %v", err)
	}

	viewers := []int64{owner.ID, voted.ID, unvoted.ID, stranger.ID}
	views, err := channels.ChannelPollFanoutViews(ctx, channelID, msgID, viewers, 1700002100)
	if err != nil {
		t.Fatalf("ChannelPollFanoutViews: %v", err)
	}
	if !views.Found {
		t.Fatal("Found=false, want true")
	}
	for _, viewer := range viewers {
		got, evaluated := views.Polls[viewer]
		if !evaluated {
			t.Fatalf("viewer %d not evaluated", viewer)
		}
		ref, err := channels.GetChannelMessages(ctx, viewer, channelID, []int{msgID})
		var refPoll *domain.MessagePoll
		if err == nil {
			for _, m := range ref.Messages {
				if m.ID == msgID && m.Media != nil {
					refPoll = m.Media.Poll
				}
			}
		}
		if (got != nil) != (refPoll != nil) {
			t.Fatalf("viewer %d visibility mismatch: batch nil=%v old nil=%v (err=%v)", viewer, got == nil, refPoll == nil, err)
		}
		if got != nil && !reflect.DeepEqual(got.Results, refPoll.Results) {
			t.Fatalf("viewer %d Results mismatch:\n batch=%+v\n  old=%+v", viewer, got.Results, refPoll.Results)
		}
	}
	// members 可见、stranger（非成员）不可见。
	if views.Polls[owner.ID] == nil || views.Polls[voted.ID] == nil || views.Polls[unvoted.ID] == nil {
		t.Fatal("members should be visible")
	}
	if views.Polls[stranger.ID] != nil {
		t.Fatal("non-member stranger must be invisible (nil)")
	}
	// voted 视角应见自己 chosen。
	sawChosen := false
	for _, v := range views.Polls[voted.ID].Results.Voters {
		if v.Chosen {
			sawChosen = true
		}
	}
	if !sawChosen {
		t.Fatal("voted viewer should see own chosen option")
	}
}
