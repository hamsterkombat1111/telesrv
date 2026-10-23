package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"telesrv/internal/domain"
)

// TransferTon moves nanoton between two accounts inside telesrv's own TON
// ledger. Both legs commit in one transaction, so a partial transfer can never
// survive a crash, and the sender's row is locked FOR UPDATE before the balance
// check so two concurrent sends cannot both spend the same nanoton.
//
// The ledger never goes negative: the sender must already cover the amount plus
// the platform fee. The returned page is the sender's post-transfer state; the
// recipient's balance is reported separately so the caller can push the matching
// update to each side.
func (s *StarGiftLifecycleStore) TransferTon(
	ctx context.Context,
	senderUserID, recipientUserID, amountNanoton, feeNanoton int64,
	date int,
) (senderBalance, recipientBalance, senderTxID, recipientTxID int64, err error) {
	if senderUserID <= 0 || recipientUserID <= 0 {
		return 0, 0, 0, 0, domain.ErrStarGiftOwnerInvalid
	}
	if senderUserID == recipientUserID {
		return 0, 0, 0, 0, domain.ErrStarGiftOwnerInvalid
	}
	if amountNanoton <= 0 || feeNanoton < 0 {
		return 0, 0, 0, 0, domain.ErrStarsInvalidAmount
	}
	if date <= 0 {
		date = int(nowUnix())
	}
	total := amountNanoton + feeNanoton

	err = withTx(ctx, s.db, "internal ton transfer", func(tx pgx.Tx) error {
		// Ensure both ledger rows exist (and apply the starting grant) before the
		// lock, so a brand-new account can transfer its first GRAM immediately.
		if _, err := s.ensureTonGrantTx(ctx, tx, senderUserID, date); err != nil {
			return err
		}
		if _, err := s.ensureTonGrantTx(ctx, tx, recipientUserID, date); err != nil {
			return err
		}

		var balance int64
		if err := tx.QueryRow(ctx,
			`SELECT balance_nanoton FROM ton_balances WHERE user_id=$1 FOR UPDATE`,
			senderUserID).Scan(&balance); err != nil {
			return err
		}
		if balance < total {
			return domain.ErrStarsInsufficient
		}

		if err := tx.QueryRow(ctx,
			`UPDATE ton_balances SET balance_nanoton=balance_nanoton-$2, updated_at=now()
			 WHERE user_id=$1 RETURNING balance_nanoton`,
			senderUserID, total).Scan(&senderBalance); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`UPDATE ton_balances SET balance_nanoton=balance_nanoton+$2, updated_at=now()
			 WHERE user_id=$1 RETURNING balance_nanoton`,
			recipientUserID, amountNanoton).Scan(&recipientBalance); err != nil {
			return err
		}

		// Sender leg is negative and carries the recipient as its peer; the
		// recipient leg is positive and carries the sender. The peer columns are
		// what wallet.getTransactions renders as the counterparty.
		if err := tx.QueryRow(ctx,
			`INSERT INTO ton_transactions(user_id,amount_nanoton,reason,peer_type,peer_id,date)
			 VALUES($1,$2,$3,'user',$4,$5) RETURNING id`,
			senderUserID, -(amountNanoton + feeNanoton), string(domain.StarsReasonGramTransfer),
			recipientUserID, date).Scan(&senderTxID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO ton_transactions(user_id,amount_nanoton,reason,peer_type,peer_id,date)
			 VALUES($1,$2,$3,'user',$4,$5) RETURNING id`,
			recipientUserID, amountNanoton, string(domain.StarsReasonGramTransfer),
			senderUserID, date).Scan(&recipientTxID); err != nil {
			return err
		}

		// The platform fee is burned rather than credited: telesrv has no fee
		// account, and silently parking it somewhere would be a lie in the ledger.
		if feeNanoton > 0 {
			if _, err := tx.Exec(ctx,
				`INSERT INTO ton_transactions(user_id,amount_nanoton,reason,date)
				 VALUES($1,$2,$3,$4)`,
				senderUserID, 0, string(domain.StarsReasonGramTransfer), date); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return senderBalance, recipientBalance, senderTxID, recipientTxID, nil
}
