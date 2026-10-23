package rpc

import (
	"context"
	"errors"
	"strconv"

	"github.com/iamxvbaba/td/tg"
	"github.com/iamxvbaba/td/tgerr"
	"github.com/iamxvbaba/td/tlprofile"
	"go.uber.org/zap"

	walletapp "telesrv/internal/app/wallet"
	"telesrv/internal/domain"
)

// registerWalletTransfer wires wallet.sendTransfer#d9e5d9e2.
//
// The official client builds and signs an external TON message and hands telesrv
// the serialized BOC. telesrv runs no TON provider by default, so it cannot
// broadcast that message -- and pretending it did would show a balance that does
// not exist on any chain. Instead the payload is parsed for its amount and
// destination, the move is settled inside telesrv's own nanoton ledger, and both
// sides get a messageActionGramTransfer#9329ea33 service message, which is
// exactly the card the client renders for a real transfer.
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
	recipientID, err := r.resolveWalletTransferRecipient(ctx, req.UserID)
	if err != nil {
		return nil, err
	}
	if recipientID == senderID {
		return nil, tgerr.New(400, "USER_ID_INVALID")
	}

	payload := req.DataNormal
	if len(payload) == 0 {
		// Gasless transfers carry the same logical message in DataGasless. Without
		// a provider there is nothing to relay either way, but the amount and
		// destination are still worth honouring.
		payload = req.DataGasless
	}
	parsed, err := walletapp.ParseTransferPayload(payload)
	if err != nil {
		return nil, tgerr.New(400, "DATA_INVALID")
	}

	const fee = int64(0) // telesrv charges no platform fee today.
	_, recipientBalance, err := r.deps.Wallet.Transfer(ctx, senderID, recipientID, parsed.Amount, fee)
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

	// The confirmation card is a service message in both directions: outbound for
	// the sender, inbound for the recipient. Failing to post it must not roll the
	// ledger back -- the money already moved -- so the error is logged and the
	// transfer still reports success.
	r.postGramTransferMessages(ctx, senderID, recipientID, parsed, recipientBalance)

	// wallet.sendTransfer answers walletState, the same shape wallet.getState
	// returns, so the client can refresh the balance from the reply.
	link, balance, ok, stateErr := r.deps.Wallet.State(ctx, senderID)
	if stateErr != nil || !ok {
		return &tg.WalletStateEmpty{}, nil
	}
	return &tg.WalletState{Address: link.Address, PublicKey: link.PublicKey, Balance: balance}, nil
}

// resolveWalletTransferRecipient turns the TL InputUser into a local user id.
func (r *Router) resolveWalletTransferRecipient(ctx context.Context, input tg.InputUserClass) (int64, error) {
	if input == nil {
		return 0, tgerr.New(400, "USER_ID_INVALID")
	}
	ids, err := r.userIDsFromInputUsers(ctx, 0, []tg.InputUserClass{input})
	if err != nil || len(ids) == 0 || ids[0] <= 0 {
		return 0, tgerr.New(400, "USER_ID_INVALID")
	}
	return ids[0], nil
}

// postGramTransferMessages writes the two service messages that render the
// transfer card. Both carry the same frozen snapshot so a replay is stable.
func (r *Router) postGramTransferMessages(ctx context.Context, senderID, recipientID int64, parsed walletapp.ParsedTransfer, recipientBalance int64) {
	if r.deps.Messages == nil {
		return
	}
	action := &domain.MessageServiceAction{
		Kind: domain.MessageServiceActionGramTransfer,
		GramTransfer: &domain.MessageGramTransferAction{
			Amount:        parsed.Amount,
			PeerAddress:   parsed.Destination,
			TransactionID: gramTransferTransactionID(senderID, recipientID, parsed.Amount),
			Comment:       parsed.Comment,
		},
	}
	media := &domain.MessageMedia{Kind: domain.MessageMediaKindService, ServiceAction: action}

	// Outbound copy in the sender's dialog.
	if _, err := r.deps.Messages.PostPrivateServiceMessage(ctx, senderID, senderID, domain.SendPrivateTextRequest{
		RecipientUserID: recipientID,
		SenderUserID:    senderID,
		RandomID:        gramTransferRandomID(senderID, recipientID, parsed.Amount),
		Media:           media,
	}); err != nil {
		r.logGramTransferMessageFailure("sender", senderID, recipientID, err)
	}
	// Inbound copy in the recipient's dialog.
	if _, err := r.deps.Messages.PostPrivateServiceMessage(ctx, recipientID, senderID, domain.SendPrivateTextRequest{
		RecipientUserID: recipientID,
		SenderUserID:    senderID,
		RandomID:        gramTransferRandomID(recipientID, senderID, parsed.Amount),
		Media:           media,
	}); err != nil {
		r.logGramTransferMessageFailure("recipient", recipientID, senderID, err)
	}
}

// logGramTransferMessageFailure records a failed card post without failing the
// transfer: the ledger already moved, and a missing card is a rendering problem
// rather than a lost payment.
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
