package memory

import (
	"context"
	"time"

	"telesrv/internal/domain"
)

// 私聊消息 poll 投票/关闭：消息可见性在本 store 校验，poll 级校验与状态全部
// 委托共享 PollStore（与 postgres 实现共用 domain 纯函数语义）。

func (s *MessageStore) VoteMessagePoll(_ context.Context, req domain.VotePrivateMessagePollRequest) (domain.PrivateMessagePollResult, error) {
	target, err := s.pollMessageTarget(req.UserID, req.Peer, req.MessageID)
	if err != nil {
		return domain.PrivateMessagePollResult{}, err
	}
	if req.Date == 0 {
		req.Date = int(time.Now().Unix())
	}
	if err := s.polls.Vote(target.Media.Poll.ID, req.UserID, req.Options, req.Date); err != nil {
		return domain.PrivateMessagePollResult{}, err
	}
	return s.privatePollResult(target.UID, target.Media.Poll.ID, req.Date), nil
}

func (s *MessageStore) CloseMessagePoll(_ context.Context, req domain.ClosePrivateMessagePollRequest) (domain.PrivateMessagePollResult, error) {
	target, err := s.pollMessageTarget(req.UserID, req.Peer, req.MessageID)
	if err != nil {
		return domain.PrivateMessagePollResult{}, err
	}
	if req.Date == 0 {
		req.Date = int(time.Now().Unix())
	}
	if err := s.polls.Close(target.Media.Poll.ID, req.UserID); err != nil {
		return domain.PrivateMessagePollResult{}, err
	}
	return s.privatePollResult(target.UID, target.Media.Poll.ID, req.Date), nil
}

// pollMessageTarget 定位 viewer box 中带 poll 的目标消息。
func (s *MessageStore) pollMessageTarget(userID int64, peer domain.Peer, messageID int) (domain.Message, error) {
	if s == nil || s.polls == nil {
		return domain.Message{}, domain.ErrMessageIDInvalid
	}
	if userID == 0 || peer.Type != domain.PeerTypeUser || peer.ID == 0 || messageID <= 0 || messageID > domain.MaxMessageBoxID {
		return domain.Message{}, domain.ErrMessageIDInvalid
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, msg := range s.m[userID] {
		if msg.ID != messageID || msg.Peer != peer {
			continue
		}
		if msg.Media == nil || msg.Media.Kind != domain.MessageMediaKindPoll || msg.Media.Poll == nil || msg.Media.Poll.ID == 0 {
			return domain.Message{}, domain.ErrMessageIDInvalid
		}
		return msg, nil
	}
	return domain.Message{}, domain.ErrMessageIDInvalid
}

// privatePollResult 收集同一 UID 的全部 owner 副本，并按各自 owner 视角 enrich poll。
func (s *MessageStore) privatePollResult(uid, pollID int64, now int) domain.PrivateMessagePollResult {
	out := domain.PrivateMessagePollResult{PollID: pollID}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, messages := range s.m {
		for _, msg := range messages {
			if msg.UID != uid {
				continue
			}
			item := cloneMessage(msg)
			item.Media = enrichPollMediaForViewer(s.polls, item.Media, item.OwnerUserID, now)
			out.Messages = append(out.Messages, item)
		}
	}
	return out
}

// enrichPrivateMessagePolls 是私聊读路径的 poll enrichment 入口（与 reactions 填充并列）。
func (s *MessageStore) enrichPrivateMessagePolls(messages []domain.Message, now int) {
	if s == nil || s.polls == nil {
		return
	}
	for i := range messages {
		messages[i].Media = enrichPollMediaForViewer(s.polls, messages[i].Media, messages[i].OwnerUserID, now)
	}
}
