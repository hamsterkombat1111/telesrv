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
	if parsed, ok := findTransferInTree(root, 0); ok {
		return parsed, nil
	}
	return ParsedTransfer{}, fmt.Errorf("%w: no internal transfer found", ErrTransferPayloadInvalid)
}

func findTransferInTree(c *cell.Cell, depth int) (ParsedTransfer, bool) {
	if c == nil || depth > 10 {
		return ParsedTransfer{}, false
	}
	var msg tlb.Message
	if err := tlb.LoadFromCell(&msg, c.MustBeginParse()); err == nil {
		if internal, ok := msg.Msg.(*tlb.InternalMessage); ok && internal != nil {
			if parsed, ok := transferFromInternal(internal); ok {
				return parsed, true
			}
		}
		if ext, ok := msg.Msg.(*tlb.ExternalMessage); ok && ext != nil && ext.Body != nil {
			if parsed, ok := findTransferInTree(ext.Body, depth+1); ok {
				return parsed, true
			}
		}
	}
	var relaxed tlb.MessageRelaxed
	if err := tlb.LoadFromCell(&relaxed, c.MustBeginParse()); err == nil {
		if parsed, ok := transferFromRelaxed(relaxed); ok {
			return parsed, true
		}
	}
	for i := 0; i < int(c.RefsNum()); i++ {
		ref, err := c.PeekRef(i)
		if err == nil && ref != nil {
			if parsed, ok := findTransferInTree(ref, depth+1); ok {
				return parsed, true
			}
		}
	}
	return ParsedTransfer{}, false
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
	amount := msg.Info.Amount
	if amount == nil || amount.Sign() <= 0 {
		return ParsedTransfer{}, false
	}
	comment := commentFromBody(msg.Body.Ref)
	return ParsedTransfer{
		Amount:      amount.Int64(),
		Destination: dst.String(),
		Comment:     comment,
	}, true
}

func transferFromInternal(msg *tlb.InternalMessage) (ParsedTransfer, bool) {
	if msg.DstAddr == nil {
		return ParsedTransfer{}, false
	}
	amount := msg.Amount.Nano()
	if amount == nil || amount.Sign() <= 0 {
		return ParsedTransfer{}, false
	}
	comment := msg.Comment()
	if comment == "" {
		comment = commentFromBody(msg.Body)
	}
	return ParsedTransfer{
		Amount:      amount.Int64(),
		Destination: msg.DstAddr.String(),
		Comment:     comment,
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
	if slice.BitsLeft() < 32 && slice.RefsNum() > 0 {
		ref, err := slice.LoadRef()
		if err == nil && ref != nil {
			slice = ref
		}
	}
	if slice.BitsLeft() < 32 {
		return ""
	}
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
