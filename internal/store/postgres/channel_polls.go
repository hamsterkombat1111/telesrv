package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"telesrv/internal/domain"
	"telesrv/internal/store/postgres/sqlcgen"
)

// 频道/超级群消息 poll 投票/关闭：成员资格与消息可见性沿用 reaction 同款校验，
// poll 级语义委托 poll.go 的共享 SQL。

func (s *ChannelStore) VoteChannelMessagePoll(ctx context.Context, req domain.VoteChannelMessagePollRequest) (domain.ChannelMessagePollResult, error) {
	return s.mutateChannelMessagePoll(ctx, req.UserID, req.ChannelID, req.MessageID, req.Date, func(ctx context.Context, tx pgx.Tx, def domain.PollDefinition, date int) error {
		return applyPollVote(ctx, tx, def, req.UserID, req.Options, date)
	})
}

func (s *ChannelStore) CloseChannelMessagePoll(ctx context.Context, req domain.CloseChannelMessagePollRequest) (domain.ChannelMessagePollResult, error) {
	return s.mutateChannelMessagePoll(ctx, req.UserID, req.ChannelID, req.MessageID, req.Date, func(ctx context.Context, tx pgx.Tx, def domain.PollDefinition, _ int) error {
		return closePollAsCreator(ctx, tx, def, req.UserID)
	})
}

func (s *ChannelStore) mutateChannelMessagePoll(
	ctx context.Context,
	userID, channelID int64,
	messageID int,
	date int,
	mutate func(ctx context.Context, tx pgx.Tx, def domain.PollDefinition, date int) error,
) (domain.ChannelMessagePollResult, error) {
	if userID == 0 || channelID == 0 || messageID <= 0 || messageID > domain.MaxMessageBoxID {
		return domain.ChannelMessagePollResult{}, domain.ErrChannelInvalid
	}
	if date == 0 {
		date = int(time.Now().Unix())
	}
	beginner, ok := s.db.(txBeginner)
	if !ok {
		return domain.ChannelMessagePollResult{}, fmt.Errorf("mutate channel message poll: db does not support transactions")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return domain.ChannelMessagePollResult{}, fmt.Errorf("begin channel poll tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	channel, member, err := s.getChannelForMember(ctx, tx, userID, channelID)
	if err != nil {
		return domain.ChannelMessagePollResult{}, err
	}
	msg, err := s.getChannelMessage(ctx, tx, channelID, messageID)
	if err != nil {
		return domain.ChannelMessagePollResult{}, err
	}
	if msg.Deleted || msg.Action != nil || msg.ID <= member.AvailableMinID {
		return domain.ChannelMessagePollResult{}, domain.ErrMessageIDInvalid
	}
	if msg.Media == nil || msg.Media.Kind != domain.MessageMediaKindPoll || msg.Media.Poll == nil || msg.Media.Poll.ID == 0 {
		return domain.ChannelMessagePollResult{}, domain.ErrMessageIDInvalid
	}
	pollID := msg.Media.Poll.ID
	defs, err := loadPollDefinitions(ctx, tx, []int64{pollID}, true)
	if err != nil {
		return domain.ChannelMessagePollResult{}, err
	}
	def, ok := defs[pollID]
	if !ok {
		return domain.ChannelMessagePollResult{}, domain.ErrPollNotFound
	}
	if err := mutate(ctx, tx, def, date); err != nil {
		return domain.ChannelMessagePollResult{}, err
	}
	if err := enrichPollMediaRefs(ctx, tx, []pollMediaRef{{media: msg.Media, viewer: userID}}); err != nil {
		return domain.ChannelMessagePollResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ChannelMessagePollResult{}, fmt.Errorf("commit channel poll tx: %w", err)
	}
	committed = true
	return domain.ChannelMessagePollResult{
		PollID:     pollID,
		Channel:    channel,
		Message:    msg,
		Recipients: []int64{userID, msg.SenderUserID},
	}, nil
}

// ChannelPollFanoutViews 批量加载一条 poll 消息对一组 viewer 的 per-viewer enrich（消除 fan-out
// 逐 viewer GetChannelMessages 的 N+1）：viewer-invariant 聚合（counts/total/recent）经
// pollViewerAggregates(viewer=0) 只算一次 + 批量 viewerOptions + 批量成员可见性，每 viewer 用与逐
// viewer 路径完全相同的 domain.ResolvePollResults 合成，故字节同源。成员/AvailableMinID 可见性在此
// 复刻 GetChannelMessages（active member && msgID>available_min_id）；bot 历史过滤由 app 层叠加。
func (s *ChannelStore) ChannelPollFanoutViews(ctx context.Context, channelID int64, msgID int, viewers []int64, now int) (domain.ChannelPollFanoutViews, error) {
	out := domain.ChannelPollFanoutViews{Polls: map[int64]*domain.MessagePoll{}}
	if channelID == 0 || msgID <= 0 || len(viewers) == 0 {
		return out, nil
	}
	msg, err := s.getChannelMessage(ctx, s.db, channelID, msgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, nil
		}
		return domain.ChannelPollFanoutViews{}, err
	}
	if msg.Deleted || msg.Media == nil || msg.Media.Kind != domain.MessageMediaKindPoll || msg.Media.Poll == nil || msg.Media.Poll.ID == 0 {
		return out, nil
	}
	pollID := msg.Media.Poll.ID
	defs, err := loadPollDefinitions(ctx, s.db, []int64{pollID}, false)
	if err != nil {
		return domain.ChannelPollFanoutViews{}, err
	}
	def, ok := defs[pollID]
	if !ok {
		return out, nil
	}
	out.Found = true
	out.Message = msg
	// viewer-invariant 模板：viewerUserID=0 → pollViewerAggregates 跳过 ViewerOptions 块，只出 counts/total/recent。
	tmplAggs, err := pollViewerAggregates(ctx, s.db, 0, []int64{pollID})
	if err != nil {
		return domain.ChannelPollFanoutViews{}, err
	}
	tmpl := tmplAggs[pollID]
	viewerOpts, err := s.batchPollViewerOptions(ctx, pollID, viewers)
	if err != nil {
		return domain.ChannelPollFanoutViews{}, err
	}
	members, err := s.batchChannelMemberAvailableMinID(ctx, channelID, viewers)
	if err != nil {
		return domain.ChannelPollFanoutViews{}, err
	}
	for _, viewer := range viewers {
		if viewer == 0 {
			continue
		}
		availMinID, isMember := members[viewer]
		if !isMember || (availMinID > 0 && msgID <= availMinID) {
			out.Polls[viewer] = nil // 已评估但不可见（非活跃成员 / pre-history 隐藏）
			continue
		}
		// agg.Counts/RecentVoters 与 tmpl 共享（ResolvePollResults 只读、RecentVoters 内部再 copy），安全。
		agg := domain.PollAggregates{
			Counts:       tmpl.Counts,
			TotalVoters:  tmpl.TotalVoters,
			RecentVoters: tmpl.RecentVoters,
			ViewerOptions: viewerOpts[viewer],
		}
		results := domain.ResolvePollResults(def, agg, viewer, now)
		pollCopy := *msg.Media.Poll // 浅拷：ApplyPollState 仅写 Closed + 新 Results 指针，不动共享 def 切片
		domain.ApplyPollState(&pollCopy, def, results, now)
		out.Polls[viewer] = &pollCopy
	}
	return out, nil
}

// batchPollViewerOptions 一次取一组 viewer 在某 poll 的投票选项（替代逐 viewer 单查）。
func (s *ChannelStore) batchPollViewerOptions(ctx context.Context, pollID int64, viewers []int64) (map[int64][][]byte, error) {
	out := make(map[int64][][]byte, len(viewers))
	if pollID == 0 || len(viewers) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT user_id, options::text FROM poll_votes WHERE poll_id = $1 AND user_id = ANY($2)`, pollID, viewers)
	if err != nil {
		return nil, fmt.Errorf("batch poll viewer options: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var userID int64
		var optionsJSON string
		if err := rows.Scan(&userID, &optionsJSON); err != nil {
			return nil, fmt.Errorf("scan poll viewer option: %w", err)
		}
		options, err := decodePollOptions(optionsJSON)
		if err != nil {
			return nil, err
		}
		out[userID] = options
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate poll viewer options: %w", err)
	}
	return out, nil
}

// batchChannelMemberAvailableMinID 一次取一组 viewer 的可见成员资格 + available_min_id；map 中存在
// = 对该频道消息可见（active 成员 && 未被 ViewMessages 限制，复刻 validateChannelMemberVisible），
// 值为其 available_min_id；缺失 = 不可见（非 active / 离开 / 被封 / ViewMessages 受限）。
func (s *ChannelStore) batchChannelMemberAvailableMinID(ctx context.Context, channelID int64, viewers []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(viewers))
	if channelID == 0 || len(viewers) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT user_id, available_min_id, banned_rights::text FROM channel_members WHERE channel_id = $1 AND user_id = ANY($2) AND status = 'active'`, channelID, viewers)
	if err != nil {
		return nil, fmt.Errorf("batch channel member available_min_id: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var userID int64
		var availableMinID int
		var bannedRights string
		if err := rows.Scan(&userID, &availableMinID, &bannedRights); err != nil {
			return nil, fmt.Errorf("scan channel member available_min_id: %w", err)
		}
		var rights domain.ChannelBannedRights
		_ = json.Unmarshal([]byte(bannedRights), &rights)
		if rights.ViewMessages {
			continue // active 但被禁止查看消息 → 不可见（与 validateChannelMemberVisible 一致）
		}
		out[userID] = availableMinID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate channel member available_min_id: %w", err)
	}
	return out, nil
}

// populateChannelMessagesPolls 把页内全部 poll media 按请求 viewer 视角 enrich；
// 由 populateChannelMessagesReactions 统一挂载（所有频道消息读路径共用一个 choke point）。
func (s *ChannelStore) populateChannelMessagesPolls(ctx context.Context, db sqlcgen.DBTX, viewerUserID int64, messages []domain.ChannelMessage) error {
	refs := make([]pollMediaRef, 0, 2)
	for i := range messages {
		refs = append(refs, pollMediaRef{media: messages[i].Media, viewer: viewerUserID})
	}
	return enrichPollMediaRefs(ctx, db, refs)
}
