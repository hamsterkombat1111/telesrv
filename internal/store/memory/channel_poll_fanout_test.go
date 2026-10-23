package memory

import (
	"context"
	"reflect"
	"testing"

	"telesrv/internal/domain"
)

// TestChannelPollFanoutViewsByteEquivalentToGetMessages 锁定 Phase 4 poll 模板化的安全网：批量
// ChannelPollFanoutViews 对每个 viewer 产出的 enrich poll，必须与逐 viewer GetChannelMessages（旧
// fan-out 路径）字节等价；且可见性判定一致（不可见 viewer 在批量里为 nil、在旧路径里取不到消息）。
// 覆盖：作者(创建者)、已投票非作者、未投票非作者、hide_results 期非创建者、late-joiner(AvailableMinID)、
// 被封成员、ViewMessages 受限成员、非成员。
func TestChannelPollFanoutViewsByteEquivalentToGetMessages(t *testing.T) {
	ctx := context.Background()
	const (
		channelID  = int64(8000000001)
		msgID      = 50
		pollID     = int64(9000000001)
		creator    = int64(7001)
		votedA     = int64(7002)
		votedB     = int64(7003)
		unvoted    = int64(7004)
		late       = int64(7005) // AvailableMinID >= msgID（pre-history 隐藏）
		banned     = int64(7006)
		restricted = int64(7007) // active 但 BannedRights.ViewMessages
		nonMember  = int64(7008)
	)
	opt1, opt2, opt3 := []byte{1}, []byte{2}, []byte{3}

	for _, hideResults := range []bool{false, true} {
		t.Run(map[bool]string{false: "visible_counts", true: "hide_results"}[hideResults], func(t *testing.T) {
			channels := NewChannelStore()
			polls := NewPollStore()
			channels.AttachPollStore(polls)

			channels.channels[channelID] = domain.Channel{ID: channelID, CreatorUserID: creator, Megagroup: true, PreHistoryHidden: true}
			channels.members[channelID] = map[int64]domain.ChannelMember{
				creator:    {UserID: creator, Status: domain.ChannelMemberActive, Role: domain.ChannelRoleCreator},
				votedA:     {UserID: votedA, Status: domain.ChannelMemberActive},
				votedB:     {UserID: votedB, Status: domain.ChannelMemberActive},
				unvoted:    {UserID: unvoted, Status: domain.ChannelMemberActive},
				late:       {UserID: late, Status: domain.ChannelMemberActive, AvailableMinID: msgID + 10},
				banned:     {UserID: banned, Status: domain.ChannelMemberBanned},
				restricted: {UserID: restricted, Status: domain.ChannelMemberActive, BannedRights: domain.ChannelBannedRights{ViewMessages: true}},
			}

			def := domain.PollDefinition{
				ID:                    pollID,
				CreatorUserID:         creator,
				Options:               [][]byte{opt1, opt2, opt3},
				PublicVoters:          true,
				MultipleChoice:        false,
				HideResultsUntilClose: hideResults,
			}
			if err := polls.CreatePoll(ctx, def); err != nil {
				t.Fatalf("create poll: %v", err)
			}
			basePoll := &domain.MessagePoll{
				ID:                    pollID,
				Question:              "Q?",
				Answers:               []domain.MessagePollAnswer{{Text: "A", Option: opt1}, {Text: "B", Option: opt2}, {Text: "C", Option: opt3}},
				PublicVoters:          true,
				HideResultsUntilClose: hideResults,
			}
			channels.messages[channelID] = []domain.ChannelMessage{{
				ChannelID:    channelID,
				ID:           msgID,
				SenderUserID: creator,
				Media:        &domain.MessageMedia{Kind: domain.MessageMediaKindPoll, Poll: basePoll},
			}}
			if err := polls.Vote(pollID, votedA, [][]byte{opt1}, 100); err != nil {
				t.Fatalf("vote A: %v", err)
			}
			if err := polls.Vote(pollID, votedB, [][]byte{opt2}, 101); err != nil {
				t.Fatalf("vote B: %v", err)
			}

			viewers := []int64{creator, votedA, votedB, unvoted, late, banned, restricted, nonMember}
			views, err := channels.ChannelPollFanoutViews(ctx, channelID, msgID, viewers, 200)
			if err != nil {
				t.Fatalf("ChannelPollFanoutViews: %v", err)
			}
			if !views.Found {
				t.Fatal("poll fan-out views Found=false, want true")
			}

			for _, viewer := range viewers {
				got, evaluated := views.Polls[viewer]
				if !evaluated {
					t.Fatalf("viewer %d not evaluated in batch (all passed viewers must be evaluated)", viewer)
				}
				// 旧路径参考：GetChannelMessages 按 viewer 取消息（含可见性 + per-viewer poll enrich）。
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
					t.Fatalf("viewer %d visibility mismatch: batch nil=%v, GetMessages nil=%v (err=%v)", viewer, got == nil, refPoll == nil, err)
				}
				if got != nil && !reflect.DeepEqual(got.Results, refPoll.Results) {
					t.Fatalf("viewer %d poll Results mismatch:\n batch=%+v\n  old=%+v", viewer, got.Results, refPoll.Results)
				}
			}

			// 显式断言可见性分类，防 GetMessages 与批量同时漏判。
			if views.Polls[creator] == nil || views.Polls[votedA] == nil || views.Polls[votedB] == nil || views.Polls[unvoted] == nil {
				t.Fatalf("active members should be visible: creator/votedA/votedB/unvoted")
			}
			if views.Polls[late] != nil || views.Polls[banned] != nil || views.Polls[restricted] != nil || views.Polls[nonMember] != nil {
				t.Fatalf("late/banned/restricted/nonMember must be invisible (nil)")
			}
			// hide_results 期：非创建者已投票者计数应被隐藏（Voters=0），创建者可见真实计数。
			if hideResults {
				for _, v := range views.Polls[votedA].Results.Voters {
					if v.Voters != 0 {
						t.Fatalf("hide_results: non-creator votedA should see Voters=0, got %d", v.Voters)
					}
				}
				sawCount := false
				for _, v := range views.Polls[creator].Results.Voters {
					if v.Voters > 0 {
						sawCount = true
					}
				}
				if !sawCount {
					t.Fatal("hide_results: creator should see real counts")
				}
			}
		})
	}
}
