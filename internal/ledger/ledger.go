// Package ledger records every movement of money as a balanced, append-only double-entry transaction.
//
// Each transaction's entries sum to zero (positive = debit, negative = credit), so money is never
// created or lost, only moved between accounts. Balances are sums of entries; nothing edits them
// directly. Postgres enforces both rules too (see migrations/00008_create_ledger.sql).
package ledger

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

type AccountType string

const (
	Asset     AccountType = "asset"     // money owed to or held by the platform, e.g. what the card network owes us
	Liability AccountType = "liability" // money the platform owes others, e.g. a merchant's balance
	Revenue   AccountType = "revenue"   // the platform's earnings, e.g. fees
)

// Account names one ledger account. Code is its unique, readable identity.
type Account struct {
	Code       string
	MerchantID *int64 // nil for platform accounts
	Livemode   bool
	Currency   string
	Type       AccountType
}

type Entry struct {
	Account Account
	Amount  int64 // positive debits, negative credits
}

type Transaction struct {
	Type        string // charge, refund, ...
	SourceID    string // the object that caused it; (Type, SourceID) is unique
	MerchantID  int64
	Livemode    bool
	Currency    string
	Description string
	Entries     []Entry
}

var ErrUnbalanced = errors.New("ledger transaction does not balance")

// Validate checks what can be checked without the database: at least two entries, no zero amounts,
// one currency, and a zero sum.
func (t Transaction) Validate() error {
	if len(t.Entries) < 2 {
		return fmt.Errorf("%w: %d entries", ErrUnbalanced, len(t.Entries))
	}
	var sum int64
	for _, e := range t.Entries {
		if e.Amount == 0 {
			return fmt.Errorf("%w: zero entry on %s", ErrUnbalanced, e.Account.Code)
		}
		if e.Account.Currency != t.Currency || e.Account.Livemode != t.Livemode {
			return fmt.Errorf("%w: account %s is not %s/livemode=%v", ErrUnbalanced, e.Account.Code, t.Currency, t.Livemode)
		}
		sum += e.Amount
	}
	if sum != 0 {
		return fmt.Errorf("%w: entries sum to %d", ErrUnbalanced, sum)
	}
	return nil
}

// Post writes t inside the caller's database transaction, so the posting commits or rolls back
// together with the business change that caused it (a PaymentIntent succeeding, a refund).
// Posting the same (Type, SourceID) twice returns the first transaction's ID and writes nothing.
func Post(ctx context.Context, tx pgx.Tx, t Transaction) (string, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}

	id := "txn_" + rand.Text()
	var existing string
	err := tx.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO ledger_transactions (id, merchant_id, livemode, currency, type, source_id, description)
			VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''))
			ON CONFLICT (type, source_id) DO NOTHING
			RETURNING id
		)
		SELECT id FROM ins
		UNION ALL
		SELECT id FROM ledger_transactions WHERE type = $5 AND source_id = $6
		LIMIT 1`,
		id, t.MerchantID, t.Livemode, t.Currency, t.Type, t.SourceID, t.Description,
	).Scan(&existing)
	if errors.Is(err, pgx.ErrNoRows) {
		// A concurrent transaction inserted the same posting after this statement's snapshot was
		// taken; ON CONFLICT waited for it to commit, so a fresh read sees it.
		err = tx.QueryRow(ctx, `SELECT id FROM ledger_transactions WHERE type = $1 AND source_id = $2`,
			t.Type, t.SourceID).Scan(&existing)
	}
	if err != nil {
		return "", err
	}
	if existing != id {
		return existing, nil
	}

	// Lock accounts in a fixed order (by code) so two concurrent postings touching the same
	// accounts cannot deadlock.
	entries := slices.Clone(t.Entries)
	slices.SortFunc(entries, func(a, b Entry) int { return strings.Compare(a.Account.Code, b.Account.Code) })
	for _, e := range entries {
		accountID, err := ensureAccount(ctx, tx, e.Account)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries (transaction_id, account_id, amount) VALUES ($1, $2, $3)`,
			id, accountID, e.Amount); err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE ledger_accounts SET balance = balance + $2 WHERE id = $1`, accountID, e.Amount); err != nil {
			return "", err
		}
	}
	return id, nil
}

// ensureAccount returns the account's ID, creating it on first use, and locks its row.
func ensureAccount(ctx context.Context, tx pgx.Tx, a Account) (int64, error) {
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_accounts (code, merchant_id, livemode, currency, type)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (code) DO NOTHING`,
		a.Code, a.MerchantID, a.Livemode, a.Currency, a.Type); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE code = $1 FOR UPDATE`, a.Code).Scan(&id)
	return id, err
}
