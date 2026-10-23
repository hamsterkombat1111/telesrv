package account

import (
	"context"
	"errors"
	"testing"

	"telesrv/internal/domain"
	"telesrv/internal/store/memory"
)

func TestBusinessProfileAndChatLinks(t *testing.T) {
	ctx := context.Background()
	const userID int64 = 1001
	store := memory.NewPasswordStore()
	svc := NewService(store, WithBusinessAutomation(store))

	profile, err := svc.UpdateBusinessWorkHours(ctx, userID, &domain.BusinessWorkHours{
		TimezoneID: "Asia/Shanghai",
		WeeklyOpen: []domain.BusinessWeeklyOpen{{
			StartMinute: 6*24*60 + 21*60,
			EndMinute:   7*24*60 + 4*60,
		}},
		OpenNow: true,
	})
	if err != nil {
		t.Fatalf("UpdateBusinessWorkHours: %v", err)
	}
	if profile.WorkHours == nil || profile.WorkHours.OpenNow {
		t.Fatalf("WorkHours = %+v, want persisted hours with OpenNow cleared", profile.WorkHours)
	}
	if _, err := svc.UpdateBusinessLocation(ctx, userID, &domain.BusinessLocation{
		Address: "No. 1 Test Road",
		Geo:     &domain.GeoPoint{Lat: 31.2, Long: 121.5},
	}); err != nil {
		t.Fatalf("UpdateBusinessLocation: %v", err)
	}
	if _, err := svc.UpdateBusinessIntro(ctx, userID, &domain.BusinessIntro{
		Title:       "Support",
		Description: "Fast replies",
	}); err != nil {
		t.Fatalf("UpdateBusinessIntro: %v", err)
	}
	got, found, err := svc.GetBusinessProfile(ctx, userID)
	if err != nil || !found {
		t.Fatalf("GetBusinessProfile found=%v err=%v", found, err)
	}
	if got.Location == nil || got.Intro == nil {
		t.Fatalf("profile = %+v, want location and intro", got)
	}

	link, err := svc.CreateBusinessChatLink(ctx, userID, domain.BusinessChatLinkInput{
		Message: "Hello from link",
		Title:   "Support link",
		Entities: []domain.MessageEntity{{
			Type:   domain.MessageEntityBold,
			Offset: 0,
			Length: 5,
		}},
	})
	if err != nil {
		t.Fatalf("CreateBusinessChatLink: %v", err)
	}
	if link.Slug == "" || link.Link == "" {
		t.Fatalf("created link = %+v, want slug/link", link)
	}
	links, err := svc.ListBusinessChatLinks(ctx, userID)
	if err != nil || len(links) != 1 {
		t.Fatalf("ListBusinessChatLinks len=%d err=%v", len(links), err)
	}
	resolved, found, err := svc.ResolveBusinessChatLink(ctx, link.Slug, true)
	if err != nil || !found || resolved.Views != 1 {
		t.Fatalf("ResolveBusinessChatLink found=%v link=%+v err=%v", found, resolved, err)
	}
	edited, err := svc.EditBusinessChatLink(ctx, userID, link.Slug, domain.BusinessChatLinkInput{
		Message: "Edited",
		Title:   "New title",
	})
	if err != nil {
		t.Fatalf("EditBusinessChatLink: %v", err)
	}
	if edited.Message != "Edited" || edited.Title != "New title" {
		t.Fatalf("edited link = %+v", edited)
	}
	deleted, err := svc.DeleteBusinessChatLink(ctx, userID, link.Slug)
	if err != nil || !deleted {
		t.Fatalf("DeleteBusinessChatLink deleted=%v err=%v", deleted, err)
	}
	if _, found, err := svc.ResolveBusinessChatLink(ctx, link.Slug, false); err != nil || found {
		t.Fatalf("Resolve deleted found=%v err=%v", found, err)
	}
}

func TestQuickRepliesLifecycle(t *testing.T) {
	ctx := context.Background()
	const userID int64 = 1002
	store := memory.NewPasswordStore()
	svc := NewService(store, WithBusinessAutomation(store))

	available, err := svc.CheckQuickReplyShortcut(ctx, userID, "hello")
	if err != nil || !available {
		t.Fatalf("CheckQuickReplyShortcut available=%v err=%v", available, err)
	}
	mutation, err := svc.SaveQuickReplyText(ctx, userID, "hello", domain.QuickReplyMessage{
		RandomID: 11,
		Date:     123,
		Message:  "First template",
		Entities: []domain.MessageEntity{{Type: domain.MessageEntityItalic, Offset: 0, Length: 5}},
	})
	if err != nil {
		t.Fatalf("SaveQuickReplyText first: %v", err)
	}
	if mutation.Kind != domain.QuickReplyMutationNew || mutation.ShortcutID == 0 || mutation.Message.ID == 0 {
		t.Fatalf("first mutation = %+v", mutation)
	}
	shortcutID := mutation.ShortcutID
	second, err := svc.SaveQuickReplyText(ctx, userID, "hello", domain.QuickReplyMessage{
		RandomID: 12,
		Date:     124,
		Message:  "Second template",
	})
	if err != nil {
		t.Fatalf("SaveQuickReplyText second: %v", err)
	}
	if second.Kind != domain.QuickReplyMutationMessage || second.ShortcutID != shortcutID {
		t.Fatalf("second mutation = %+v", second)
	}
	if available, err := svc.CheckQuickReplyShortcut(ctx, userID, "hello"); err != nil || available {
		t.Fatalf("CheckQuickReplyShortcut duplicate available=%v err=%v", available, err)
	}
	list, err := svc.ListQuickReplies(ctx, userID)
	if err != nil || len(list.QuickReplies) != 1 || list.QuickReplies[0].Count != 2 || list.Hash == 0 {
		t.Fatalf("ListQuickReplies = %+v err=%v", list, err)
	}
	msgs, err := svc.GetQuickReplyMessages(ctx, userID, shortcutID, nil)
	if err != nil || msgs.Count != 2 || len(msgs.Messages) != 2 || msgs.Hash == 0 {
		t.Fatalf("GetQuickReplyMessages = %+v err=%v", msgs, err)
	}
	if _, err := svc.RenameQuickReplyShortcut(ctx, userID, shortcutID, "renamed"); err != nil {
		t.Fatalf("RenameQuickReplyShortcut: %v", err)
	}
	if _, err := svc.ReorderQuickReplies(ctx, userID, []int{shortcutID}); err != nil {
		t.Fatalf("ReorderQuickReplies: %v", err)
	}
	deleteMutation, err := svc.DeleteQuickReplyMessages(ctx, userID, shortcutID, []int{msgs.Messages[0].ID})
	if err != nil {
		t.Fatalf("DeleteQuickReplyMessages: %v", err)
	}
	if deleteMutation.Kind != domain.QuickReplyMutationIDs || len(deleteMutation.MessageIDs) != 1 {
		t.Fatalf("delete mutation = %+v", deleteMutation)
	}
	if _, err := svc.DeleteQuickReplyShortcut(ctx, userID, shortcutID); err != nil {
		t.Fatalf("DeleteQuickReplyShortcut: %v", err)
	}
	if _, err := svc.GetQuickReplyMessages(ctx, userID, shortcutID, nil); !errors.Is(err, domain.ErrShortcutInvalid) {
		t.Fatalf("GetQuickReplyMessages deleted err = %v, want ErrShortcutInvalid", err)
	}
}
