package rpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/iamxvbaba/td/tg"
	"github.com/iamxvbaba/td/tgerr"
	"github.com/iamxvbaba/td/tlprofile"
	"go.uber.org/zap"

	walletapp "telesrv/internal/app/wallet"
	"telesrv/internal/domain"
)

type walletAddressLookup interface {
	LinksByAddresses(ctx context.Context, addrs []string) ([]domain.WalletLink, error)
}

// registerWalletTransfer wires wallet.sendTransfer#d9e5d9e2.
//
// The official client builds and signs an external TON message and hands telesrv
// the serialized BOC. telesrv settles the move inside its own nanoton ledger,
// posts a messageActionGramTransfer#9329ea33 service message to the dialog, and
// returns Updates containing UpdateMessageID, UpdateNewMessage,
// UpdateSentWalletTransaction, and UpdateWalletState.
func (r *Router) registerWalletTransfer(d *tlprofile.Dispatcher) {
	registerRPC[*tg.WalletSendTransferRequest](d, tlprofile.SemanticMethodWalletSendTransfer, func(ctx context.Context, req *tg.WalletSendTransferRequest) (any, error) {
		return r.onWalletSendTransfer(ctx, req)
	})
}

func (r *Router) onWalletSendTransfer(ctx context.Context, req *tg.WalletSendTransferRequest) (any, error) {
	senderID, _, err := r.walletCaller(ctx, "send-transfer", 30)
	if err != nil {
		return nil, err
	}
	if r.deps.Wallet == nil {
		return nil, walletRPCError(walletapp.ErrProviderUnavailable)
	}

	payload := req.DataNormal
	gasless := false
	if len(payload) == 0 {
		payload = req.DataGasless
		gasless = true
	}
	parsed, err := walletapp.ParseTransferPayload(payload)
	if err != nil {
		if r.log != nil {
			r.log.Warn("wallet.sendTransfer invalid payload", zap.Int("len", len(payload)), zap.Error(err))
		}
		return nil, tgerr.New(400, "DATA_INVALID")
	}

	recipientID, err := r.resolveWalletTransferRecipient(ctx, senderID, req.UserID, parsed.Destination)
	if err != nil {
		if r.log != nil {
			r.log.Warn("wallet.sendTransfer invalid recipient",
				zap.Int64("sender_id", senderID),
				zap.String("dest", parsed.Destination),
				zap.Error(err),
			)
		}
		return nil, err
	}
	if recipientID == senderID {
		return nil, tgerr.New(400, "USER_ID_INVALID")
	}

	const fee = int64(0) // telesrv charges no platform fee today.
	page, recipientBalance, err := r.deps.Wallet.Transfer(ctx, senderID, recipientID, parsed.Amount, fee)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrStarsInsufficient):
			return nil, tgerr.New(400, "BALANCE_TOO_LOW")
		case errors.Is(err, domain.ErrStarsInvalidAmount):
			return nil, tgerr.New(400, "AMOUNT_INVALID")
		case errors.Is(err, walletapp.ErrProviderUnavailable):
			return nil, tgerr.New(503, "WALLET_PROVIDER_UNAVAILABLE")
		default:
			return nil, internalErr()
		}
	}

	now := int(r.clock.Now().Unix())
	txID := gramTransferTransactionID(senderID, recipientID, parsed.Amount)
	if len(page.Transactions) > 0 && page.Transactions[0].ID > 0 {
		txID = strconv.FormatInt(page.Transactions[0].ID, 10)
	}
	sum := sha256.Sum256(payload)
	msgHash := hex.EncodeToString(sum[:])

	updates := r.postGramTransferMessages(ctx, senderID, recipientID, req.RandomID, txID, parsed, recipientBalance, now)

	peerAddress := parsed.Destination
	if links, lErr := r.deps.Wallet.Links(ctx, []int64{recipientID}); lErr == nil && len(links) > 0 && links[0].Address != "" {
		peerAddress = links[0].Address
	}

	tx := tg.WalletTransaction{
		Incoming: false,
		Gasless:  gasless,
		Failed:   false,
		ID:       txID,
		Amount:   parsed.Amount,
		Fee:      fee,
		Date:     now,
		Peer: &tg.WalletTransactionPeerUser{
			UserID:  recipientID,
			Address: peerAddress,
		},
	}
	if parsed.Comment != "" {
		tx.SetComment(parsed.Comment)
	}
	tx.SetTxHash(msgHash)

	sentUpdate := &tg.UpdateSentWalletTransaction{
		Gasless: gasless,
		MsgHash: msgHash,
	}
	sentUpdate.SetTransaction(tx)
	updates.Updates = append(updates.Updates, sentUpdate)

	if link, balance, ok, stateErr := r.deps.Wallet.State(ctx, senderID); stateErr == nil && ok {
		updates.Updates = append(updates.Updates, &tg.UpdateWalletState{
			State: &tg.WalletState{
				Address:   link.Address,
				PublicKey: link.PublicKey,
				Balance:   balance,
			},
		})
	}
	return updates, nil
}

// resolveWalletTransferRecipient turns the TL InputUser or destination TON address into a local user id.
func (r *Router) resolveWalletTransferRecipient(ctx context.Context, senderID int64, input tg.InputUserClass, destinationAddr string) (int64, error) {
	if input != nil {
		if _, isEmpty := input.(*tg.InputUserEmpty); !isEmpty {
			ids, err := r.userIDsFromInputUsers(ctx, senderID, []tg.InputUserClass{input})
			if err == nil && len(ids) > 0 && ids[0] > 0 {
				return ids[0], nil
			}
			if u, ok := input.(*tg.InputUser); ok && u.UserID > 0 && r.deps.Users != nil {
				user, found, uErr := r.deps.Users.AdminUser(ctx, u.UserID)
				if uErr == nil && found && !user.Deleted && user.ID > 0 {
					return user.ID, nil
				}
			}
		}
	}
	if destinationAddr != "" && r.deps.Wallet != nil {
		if lookup, ok := r.deps.Wallet.(walletAddressLookup); ok {
			links, err := lookup.LinksByAddresses(ctx, []string{destinationAddr})
			if err == nil && len(links) > 0 && links[0].UserID > 0 {
				return links[0].UserID, nil
			}
		}
	}
	return 0, tgerr.New(400, "USER_ID_INVALID")
}

// postGramTransferMessages writes the private service message for both sender and recipient
// and builds the sender's Updates response.
func (r *Router) postGramTransferMessages(ctx context.Context, senderID, recipientID, reqRandomID int64, txID string, parsed walletapp.ParsedTransfer, _ int64, now int) *tg.Updates {
	emptyUpdates := &tg.Updates{
		Updates: []tg.UpdateClass{},
		Users:   []tg.UserClass{},
		Chats:   []tg.ChatClass{},
		Date:    now,
	}
	if r.deps.Messages == nil {
		return emptyUpdates
	}
	peerAddress := parsed.Destination
	if r.deps.Wallet != nil {
		if links, err := r.deps.Wallet.Links(ctx, []int64{recipientID}); err == nil && len(links) > 0 && links[0].Address != "" {
			peerAddress = links[0].Address
		}
	}
	action := &domain.MessageServiceAction{
		Kind: domain.MessageServiceActionGramTransfer,
		GramTransfer: &domain.MessageGramTransferAction{
			Amount:        parsed.Amount,
			PeerAddress:   peerAddress,
			TransactionID: txID,
			Comment:       parsed.Comment,
		},
	}
	media := &domain.MessageMedia{Kind: domain.MessageMediaKindService, ServiceAction: action}

	randomID := reqRandomID
	if randomID == 0 {
		randomID = gramTransferRandomID(senderID, recipientID, parsed.Amount) ^ int64(now)
	}
	sessionID, _ := SessionIDFrom(ctx)
	res, err := r.deps.Messages.PostPrivateServiceMessage(ctx, recipientID, senderID, domain.SendPrivateTextRequest{
		RecipientUserID: recipientID,
		SenderUserID:    senderID,
		RandomID:        randomID,
		Media:           media,
		Date:            now,
		OriginAuthKeyID: rawAuthKeyIDForOrigin(ctx),
		OriginSessionID: sessionID,
	})
	if err != nil {
		r.logGramTransferMessageFailure("transfer", senderID, recipientID, err)
		return emptyUpdates
	}
	if res.SenderMessage.ID <= 0 {
		return emptyUpdates
	}
	users := r.usersForMessageUpdate(ctx, senderID, res.SenderMessage)
	chats := r.chatsForMessageUpdate(ctx, senderID, res.SenderMessage)
	return tgPrivateSendResultUpdates(res, reqRandomID, reqRandomID != 0, users, chats)
}

func (r *Router) logGramTransferMessageFailure(side string, ownerID, peerID int64, err error) {
	if r.log == nil {
		return
	}
	r.log.Warn("gram transfer service message not posted",
		zap.String("side", side), zap.Int64("owner", ownerID), zap.Int64("peer", peerID), zap.Error(err))
}

func gramTransferTransactionID(senderID, recipientID, amount int64) string {
	return "gram:" + strconv.FormatInt(senderID, 10) + ":" + strconv.FormatInt(recipientID, 10) + ":" + strconv.FormatInt(amount, 10)
}

func gramTransferRandomID(ownerID, peerID, amount int64) int64 {
	var id int64 = 0x4752414d54524e
	id ^= ownerID << 19
	id ^= peerID << 5
	id ^= amount
	if id == 0 {
		return 0x47524101
	}
	return id
}
