package wallet

import (
	"math/big"
	"testing"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

func TestParseTransferPayloadTree(t *testing.T) {
	dst, err := address.ParseAddr("EQAJF6HsBWCk6nCuQydNo6910USpwONIqfBH7FPKWQDzhGn6")
	if err != nil {
		t.Fatal(err)
	}

	bodyCell := cell.BeginCell().MustStoreUInt(0, 32).MustStoreStringSnake("test comment").EndCell()

	intMsg := &tlb.InternalMessage{
		IHRDisabled: true,
		Bounce:      false,
		Bounced:     false,
		SrcAddr:     nil,
		DstAddr:     dst,
		Amount:      tlb.FromNanoTON(big.NewInt(22000000000)),
		Body:        cell.BeginCell().MustStoreRef(bodyCell).EndCell(),
	}

	intCell, err := tlb.ToCell(intMsg)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Direct InternalMessage
	parsed, err := ParseTransferPayload(intCell.ToBOC())
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	if parsed.Amount != 22000000000 {
		t.Fatalf("expected 22000000000, got %d", parsed.Amount)
	}
	if parsed.Comment != "test comment" {
		t.Fatalf("expected test comment, got %s", parsed.Comment)
	}

	// 2. Wrapped inside an ExternalMessage (like client produces)
	extMsg := &tlb.ExternalMessage{
		DstAddr: dst,
		Body:    cell.BeginCell().MustStoreRef(intCell).EndCell(),
	}
	extCell, err := tlb.ToCell(extMsg)
	if err != nil {
		t.Fatal(err)
	}

	parsedExt, err := ParseTransferPayload(extCell.ToBOC())
	if err != nil {
		t.Fatalf("external wrapper: %v", err)
	}
	if parsedExt.Amount != 22000000000 {
		t.Fatalf("expected 22000000000, got %d", parsedExt.Amount)
	}
	if parsedExt.Comment != "test comment" {
		t.Fatalf("expected test comment, got %s", parsedExt.Comment)
	}
}
