package wallet

import (
	"errors"
	"fmt"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"

	"telesrv/internal/domain"
)

// ErrTransferPayloadInvalid marks a wallet.sendTransfer payload that is not a
// well-formed, internally-consistent transfer. It is separate from a signature
// failure: with no provider configured telesrv cannot verify a signature at all,
// so it must reject anything it cannot parse rather than settle blind.
var ErrTransferPayloadInvalid = errors.New("wallet: transfer payload invalid")

// ParsedTransfer is the subset of a signed transfer telesrv needs in order to
// move nanoton in its own ledger.
type ParsedTransfer struct {
	// Amount is the nanoton value the sender attached to the message.
	Amount int64
	// Destination is the recipient's TON address in its raw form.
	Destination string
	// Comment is the plain-text comment, if the payload carried one.
	Comment string
}

// ParseTransferPayload extracts the amount, destination and comment from the
// BOC a client sends in wallet.sendTransfer.
//
// The client builds a signed external message whose body is an internal message
// carrying the transfer. telesrv does not broadcast it (no provider is
// configured), but it still parses the value so the ledger move matches what the
// user saw in the confirmation sheet. Anything that does not parse is rejected:
// settling a transfer whose amount could not be read would move the wrong money.
func ParseTransferPayload(boc []byte) (ParsedTransfer, error) {
	if len(boc) == 0 {
		return ParsedTransfer{}, ErrTransferPayloadInvalid
	}
	root, err := cell.FromBOC(boc)
	if err != nil {
		return ParsedTransfer{}, fmt.Errorf("%w: %v", ErrTransferPayloadInvalid, err)
	}
	return parseTransferCell(root)
}

func parseTransferCell(root *cell.Cell) (ParsedTransfer, error) {
	// The signed message is an external-in message whose body is either the
	// internal transfer directly or a wallet-v5 action list wrapping it. Try the
	// relaxed message form first because that is what the current wallet
	// produces, then fall back to a bare internal message.
	var relaxed tlb.MessageRelaxed
	if err := tlb.LoadFromCell(&relaxed, root.MustBeginParse()); err == nil {
		if parsed, ok := transferFromRelaxed(relaxed); ok {
			return parsed, nil
		}
	}
	var msg tlb.Message
	if err := tlb.LoadFromCell(&msg, root.MustBeginParse()); err == nil {
		if internal, ok := msg.Msg.(*tlb.InternalMessage); ok && internal != nil {
			if parsed, ok := transferFromInternal(internal); ok {
				return parsed, nil
			}
		}
	}
	return ParsedTransfer{}, fmt.Errorf("%w: no internal transfer found", ErrTransferPayloadInvalid)
}

func transferFromRelaxed(msg tlb.MessageRelaxed) (ParsedTransfer, bool) {
	if msg.MsgType != tlb.MsgTypeInternal && msg.MsgType != "" {
		// External-in wrappers carry the internal message in their body; that
		// shape is handled by the tlb.Message fallback.
		return ParsedTransfer{}, false
	}
	dst := msg.Info.DstAddr
	if dst == nil {
		return ParsedTransfer{}, false
	}
	amount := msg.Info.Amount.Nano()
	if amount <= 0 {
		return ParsedTransfer{}, false
	}
	return ParsedTransfer{
		Amount:      amount.Int64(),
		Destination: dst.String(),
		Comment:     commentFromBody(msg.Body.Body),
	}, true
}

func transferFromInternal(msg *tlb.InternalMessage) (ParsedTransfer, bool) {
	if msg.DstAddr == nil {
		return ParsedTransfer{}, false
	}
	amount := msg.Amount.Nano()
	if amount <= 0 {
		return ParsedTransfer{}, false
	}
	return ParsedTransfer{
		Amount:      amount.Int64(),
		Destination: msg.DstAddr.String(),
		Comment:     commentFromBody(msg.Body),
	}, true
}

// commentFromBody reads the conventional TON "zero opcode + UTF-8 string" text
// comment body. Anything else (jetton transfers, forward payloads) is reported
// as no comment rather than misparsed as text.
func commentFromBody(body *cell.Cell) string {
	if body == nil {
		return ""
	}
	slice := body.MustBeginParse()
	op, err := slice.LoadUInt(32)
	if err != nil || op != 0 {
		return ""
	}
	text, err := slice.LoadStringSnake()
	if err != nil {
		return ""
	}
	if len(text) > domain.MaxGramTransferCommentBytes {
		text = text[:domain.MaxGramTransferCommentBytes]
	}
	return text
}

// DestinationAddress parses the destination so the RPC layer can reject payloads
// aimed at an address the recipient does not own.
func DestinationAddress(raw string) (*address.Address, error) {
	if raw == "" {
		return nil, ErrTransferPayloadInvalid
	}
	addr, err := address.ParseAddr(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransferPayloadInvalid, err)
	}
	return addr, nil
}
