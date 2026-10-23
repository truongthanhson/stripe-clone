package ledger

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongthanhson/stripe-clone/internal/listing"
)

var ErrNotFound = errors.New("balance transaction not found")

// Reader answers merchants' questions about their money, from the ledger.
type Reader struct {
	pool *pgxpool.Pool
}

func NewReader(pool *pgxpool.Pool) *Reader {
	return &Reader{pool: pool}
}

// Balance is a merchant's money in one currency, from the merchant's side: positive means the
// platform owes it to them. Pending is not yet available for payout.
type Balance struct {
	Currency  string
	Available int64
	Pending   int64
}

// Balances reads the merchant's accounts. A merchant account is a liability, so its ledger balance
// is negative (a credit) when the platform owes the merchant; the sign is flipped for the merchant.
func (r *Reader) Balances(ctx context.Context, merchantID int64, livemode bool) ([]Balance, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT currency, code, balance FROM ledger_accounts
		 WHERE merchant_id = $1 AND livemode = $2 ORDER BY currency, code`, merchantID, livemode)
	if err != nil {
		return nil, err
	}
	var out []Balance
	var currency, code string
	var balance int64
	_, err = pgx.ForEachRow(rows, []any{&currency, &code, &balance}, func() error {
		if len(out) == 0 || out[len(out)-1].Currency != currency {
			out = append(out, Balance{Currency: currency})
		}
		b := &out[len(out)-1]
		switch {
		case strings.HasSuffix(code, ":pending"):
			b.Pending += -balance
		case strings.HasSuffix(code, ":available"):
			b.Available += -balance
		}
		return nil
	})
	return out, err
}

// BalanceTransaction is one ledger transaction as the merchant sees it: what it did to their balance.
type BalanceTransaction struct {
	ID          string
	Type        string // charge, refund
	Source      string // the pi_ or re_ behind it
	Currency    string
	Description string
	Amount      int64 // gross: Net + Fee
	Fee         int64 // kept by the platform
	Net         int64 // change to the merchant's balance
	Created     time.Time
}

// balanceTransactions derives amount, fee and net from the entries: net is what moved on the
// merchant's accounts, fee is what moved to platform revenue.
const balanceTransactions = `
	SELECT id, type, source_id, currency, description, created_at, net, fee FROM (
		SELECT t.id, t.type, t.source_id, t.currency, COALESCE(t.description, '') AS description, t.created_at,
		       t.merchant_id, t.livemode,
		       COALESCE(-SUM(e.amount) FILTER (WHERE a.merchant_id IS NOT NULL), 0) AS net,
		       COALESCE(-SUM(e.amount) FILTER (WHERE a.type = 'revenue'), 0) AS fee
		FROM ledger_transactions t
		JOIN ledger_entries e ON e.transaction_id = t.id
		JOIN ledger_accounts a ON a.id = e.account_id
		GROUP BY t.id
	) bt WHERE merchant_id = $1 AND livemode = $2`

func scanBalanceTransaction(row pgx.Row) (BalanceTransaction, error) {
	var bt BalanceTransaction
	err := row.Scan(&bt.ID, &bt.Type, &bt.Source, &bt.Currency, &bt.Description, &bt.Created, &bt.Net, &bt.Fee)
	if errors.Is(err, pgx.ErrNoRows) {
		return BalanceTransaction{}, ErrNotFound
	}
	bt.Amount = bt.Net + bt.Fee
	return bt, err
}

func (r *Reader) GetTransaction(ctx context.Context, merchantID int64, livemode bool, id string) (BalanceTransaction, error) {
	return scanBalanceTransaction(r.pool.QueryRow(ctx, balanceTransactions+` AND id = $3`, merchantID, livemode, id))
}

func (r *Reader) ListTransactions(ctx context.Context, merchantID int64, livemode bool, p listing.Params) ([]BalanceTransaction, bool, error) {
	args := []any{merchantID, livemode}
	var cursor BalanceTransaction
	if id, _ := p.Cursor(); id != "" {
		var err error
		if cursor, err = r.GetTransaction(ctx, merchantID, livemode, id); err != nil {
			return nil, false, err
		}
	}
	query, args := p.Keyset(balanceTransactions, args, cursor.Created, cursor.ID)
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	page, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (BalanceTransaction, error) {
		return scanBalanceTransaction(row)
	})
	if err != nil {
		return nil, false, err
	}
	page, hasMore := listing.Trim(p, page)
	return page, hasMore, nil
}
