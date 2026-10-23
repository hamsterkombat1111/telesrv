package postgres

import (
	"context"
	"fmt"
	"strings"
	"telesrv/internal/domain"
)

func (s *ChannelStore) ListInviteImporters(ctx context.Context, req domain.ChannelInviteImportersRequest) (domain.ChannelInviteImporterList, error) {
	if req.UserID == 0 || req.ChannelID == 0 {
		return domain.ChannelInviteImporterList{}, domain.ErrChannelInvalid
	}
	if _, member, err := s.getChannelForMember(ctx, s.db, req.UserID, req.ChannelID); err != nil {
		return domain.ChannelInviteImporterList{}, err
	} else if !canExportChannelInvite(member) {
		return domain.ChannelInviteImporterList{}, domain.ErrChannelAdminRequired
	}
	var inviteID int64
	if req.Hash != "" {
		invite, err := s.getInviteByChannelHash(ctx, s.db, req.ChannelID, req.Hash, false)
		if err != nil {
			return domain.ChannelInviteImporterList{}, err
		}
		inviteID = invite.InviteID
	}
	limit := req.Limit
	if limit <= 0 || limit > domain.MaxChannelInviteListLimit {
		limit = domain.MaxChannelInviteListLimit
	}
	args := []any{req.ChannelID, req.Requested, inviteID, req.Query, req.OffsetDate, req.OffsetUserID, limit}
	where := []string{
		"i.channel_id = $1",
		"i.requested = $2",
		"($3::bigint = 0 OR i.invite_id = $3)",
		"($4::text = '' OR lower(trim(u.username || ' ' || u.first_name || ' ' || u.last_name)) LIKE '%' || lower($4) || '%')",
		"(($5::int = 0 AND $6::bigint = 0) OR (i.date, i.user_id) < ($5, $6))",
	}
	whereSQL := strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRow(ctx, `
SELECT COUNT(*)::int
FROM channel_invite_importers i
JOIN users u ON u.id = i.user_id
WHERE `+whereSQL, args[:6]...).Scan(&total); err != nil {
		return domain.ChannelInviteImporterList{}, err
	}
	rows, err := s.db.Query(ctx, `
SELECT i.channel_id, i.invite_id, i.user_id, i.date, i.requested, i.approved_by, i.via_chatlist, i.about
FROM channel_invite_importers i
JOIN users u ON u.id = i.user_id
WHERE `+whereSQL+`
ORDER BY i.date DESC, i.user_id DESC
LIMIT $7`, args...)
	if err != nil {
		return domain.ChannelInviteImporterList{}, err
	}
	defer rows.Close()
	importers := make([]domain.ChannelInviteImporter, 0, limit)
	for rows.Next() {
		var importer domain.ChannelInviteImporter
		if err := rows.Scan(&importer.ChannelID, &importer.InviteID, &importer.UserID, &importer.Date, &importer.Requested, &importer.ApprovedBy, &importer.ViaChatlist, &importer.About); err != nil {
			return domain.ChannelInviteImporterList{}, err
		}
		importers = append(importers, importer)
	}
	if err := rows.Err(); err != nil {
		return domain.ChannelInviteImporterList{}, err
	}
	return domain.ChannelInviteImporterList{Count: total, Importers: importers}, nil
}

func (s *ChannelStore) PendingJoinRequests(ctx context.Context, channelID int64, limit int) (domain.ChannelPendingJoinRequests, error) {
	if channelID == 0 {
		return domain.ChannelPendingJoinRequests{}, domain.ErrChannelInvalid
	}
	if limit <= 0 || limit > domain.MaxChannelPendingJoinRecentRequesters {
		limit = domain.MaxChannelPendingJoinRecentRequesters
	}
	rows, err := s.db.Query(ctx, `
SELECT user_id, COUNT(*) OVER()::int
FROM channel_invite_importers
WHERE channel_id = $1 AND requested
ORDER BY date DESC, user_id DESC
LIMIT $2`, channelID, limit)
	if err != nil {
		return domain.ChannelPendingJoinRequests{}, fmt.Errorf("list pending channel join requests: %w", err)
	}
	defer rows.Close()
	out := domain.ChannelPendingJoinRequests{
		ChannelID:        channelID,
		RecentRequesters: make([]int64, 0, limit),
	}
	for rows.Next() {
		var userID int64
		var count int
		if err := rows.Scan(&userID, &count); err != nil {
			return domain.ChannelPendingJoinRequests{}, err
		}
		out.Count = count
		out.RecentRequesters = append(out.RecentRequesters, userID)
	}
	if err := rows.Err(); err != nil {
		return domain.ChannelPendingJoinRequests{}, err
	}
	return out, nil
}

func (s *ChannelStore) HideAllChatJoinRequests(ctx context.Context, req domain.HideChannelJoinRequestsRequest) (domain.CreateChannelResult, error) {
	if req.UserID == 0 || req.ChannelID == 0 {
		return domain.CreateChannelResult{}, domain.ErrChannelInvalid
	}
	if req.Date == 0 {
		req.Date = nowUnix()
	}
	var inviteID int64
	if req.Hash != "" {
		invite, err := s.GetExportedInvite(ctx, domain.GetChannelInviteRequest{UserID: req.UserID, ChannelID: req.ChannelID, Hash: req.Hash})
		if err != nil {
			return domain.CreateChannelResult{}, err
		}
		inviteID = invite.InviteID
	}
	limit := req.Limit
	if limit <= 0 || limit > domain.MaxChannelHideJoinRequests {
		limit = domain.MaxChannelHideJoinRequests
	}
	rows, err := s.db.Query(ctx, `
SELECT user_id
FROM channel_invite_importers
WHERE channel_id = $1 AND requested AND ($2::bigint = 0 OR invite_id = $2)
ORDER BY date ASC, user_id ASC
LIMIT $3`, req.ChannelID, inviteID, limit)
	if err != nil {
		return domain.CreateChannelResult{}, err
	}
	targets := make([]int64, 0, limit)
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			rows.Close()
			return domain.CreateChannelResult{}, err
		}
		targets = append(targets, userID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return domain.CreateChannelResult{}, err
	}
	rows.Close()
	var result domain.CreateChannelResult
	for _, target := range targets {
		next, err := s.HideChatJoinRequest(ctx, domain.HideChannelJoinRequestRequest{
			UserID:       req.UserID,
			ChannelID:    req.ChannelID,
			TargetUserID: target,
			Approved:     req.Approved,
			Date:         req.Date,
		})
		if err != nil {
			return domain.CreateChannelResult{}, err
		}
		result = next
	}
	if result.Channel.ID == 0 {
		ch, _, err := s.getChannelForMember(ctx, s.db, req.UserID, req.ChannelID)
		if err != nil {
			return domain.CreateChannelResult{}, err
		}
		result.Channel = ch
		recipients, _ := s.ListActiveChannelMemberIDs(ctx, req.UserID, req.ChannelID, 0)
		result.Recipients = recipients
	}
	return result, nil
}

func (s *ChannelStore) ListChannelInviteAdminMemberIDs(ctx context.Context, channelID int64, limit int) ([]int64, error) {
	if channelID == 0 {
		return nil, domain.ErrChannelInvalid
	}
	if limit <= 0 || limit > domain.MaxChannelRealtimeFanout {
		limit = domain.MaxChannelRealtimeFanout
	}
	rows, err := s.db.Query(ctx, `
SELECT user_id
FROM channel_members
WHERE channel_id = $1
  AND status = 'active'
  AND (
    role = 'creator' OR
    (role = 'admin' AND (
      (admin_rights->>'InviteUsers')::boolean IS TRUE OR
      (admin_rights->>'ChangeInfo')::boolean IS TRUE
    ))
  )
ORDER BY user_id
LIMIT $2`, channelID, limit)
	if err != nil {
		return nil, fmt.Errorf("list channel invite admin members: %w", err)
	}
	defer rows.Close()
	out := make([]int64, 0, limit)
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			return nil, err
		}
		out = append(out, userID)
	}
	return out, rows.Err()
}
