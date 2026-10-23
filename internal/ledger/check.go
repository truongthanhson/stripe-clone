package ledger

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Check verifies the ledger's invariants over all history: every transaction sums to zero, and every
// account's cached balance equals the sum of its entries. It returns one line per violation.
// Tests run it after every scenario; an operator can run it at any time.
func Check(ctx context.Context, db querier) ([]string, error) {
	var problems []string

	rows, err := db.Query(ctx, `
		SELECT t.id, COALESCE(SUM(e.amount), 0)
		FROM ledger_transactions t LEFT JOIN ledger_entries e ON e.transaction_id = t.id
		GROUP BY t.id HAVING COALESCE(SUM(e.amount), 0) <> 0 OR COUNT(e.id) < 2`)
	if err != nil {
		return nil, err
	}
	var id string
	var sum int64
	if _, err := pgx.ForEachRow(rows, []any{&id, &sum}, func() error {
		problems = append(problems, fmt.Sprintf("transaction %s: entries sum to %d", id, sum))
		return nil
	}); err != nil {
		return nil, err
	}

	rows, err = db.Query(ctx, `
		SELECT a.code, a.balance, COALESCE(SUM(e.amount), 0)
		FROM ledger_accounts a LEFT JOIN ledger_entries e ON e.account_id = a.id
		GROUP BY a.id HAVING a.balance <> COALESCE(SUM(e.amount), 0)`)
	if err != nil {
		return nil, err
	}
	var code string
	var cached, derived int64
	if _, err := pgx.ForEachRow(rows, []any{&code, &cached, &derived}, func() error {
		problems = append(problems, fmt.Sprintf("account %s: cached balance %d, entries sum to %d", code, cached, derived))
		return nil
	}); err != nil {
		return nil, err
	}
	return problems, nil
}
