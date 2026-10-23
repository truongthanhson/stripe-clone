package ledger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongthanhson/stripe-clone/internal/apikey"
	"github.com/truongthanhson/stripe-clone/internal/ledger"
	"github.com/truongthanhson/stripe-clone/internal/testdb"
)

func post(t *testing.T, pool *pgxpool.Pool, tx ledger.Transaction) (string, error) {
	t.Helper()
	var id string
	err := pgx.BeginFunc(context.Background(), pool, func(dbtx pgx.Tx) error {
		var err error
		id, err = ledger.Post(context.Background(), dbtx, tx)
		return err
	})
	return id, err
}

func TestPost(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t)
	merchant, err := apikey.NewStore(pool).CreateMerchant(ctx, "Acme", "ops@acme.test")
	if err != nil {
		t.Fatal(err)
	}
	balance := func(code string) int64 {
		var b int64
		_ = pool.QueryRow(ctx, `SELECT balance FROM ledger_accounts WHERE code = $1`, code).Scan(&b)
		return b
	}
	check := func(t *testing.T) {
		t.Helper()
		problems, err := ledger.Check(ctx, pool)
		if err != nil || len(problems) > 0 {
			t.Fatalf("ledger check: %v %v", err, problems)
		}
	}

	t.Run("a charge moves money between three accounts and balances", func(t *testing.T) {
		id, err := post(t, pool, ledger.Charge("pi_1", merchant, false, "usd", 2000))
		if err != nil || !strings.HasPrefix(id, "txn_") {
			t.Fatalf("post = %q, %v", id, err)
		}
		if got := balance(ledger.NetworkReceivable(false, "usd").Code); got != 2000 {
			t.Errorf("network receivable = %d", got)
		}
		if got := balance(ledger.MerchantPending(merchant, false, "usd").Code); got != -1912 {
			t.Errorf("merchant pending = %d, want -1912 (a credit: we owe it)", got)
		}
		if got := balance(ledger.PlatformFees(false, "usd").Code); got != -88 {
			t.Errorf("fees = %d", got)
		}
		check(t)
	})

	t.Run("posting the same source twice is a no-op", func(t *testing.T) {
		first, _ := post(t, pool, ledger.Charge("pi_2", merchant, false, "usd", 1000))
		second, err := post(t, pool, ledger.Charge("pi_2", merchant, false, "usd", 1000))
		if err != nil || second != first {
			t.Fatalf("second post = %q, %v; want %q", second, err, first)
		}
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM ledger_entries e JOIN ledger_transactions t ON t.id = e.transaction_id WHERE t.source_id = 'pi_2'`).Scan(&n)
		if n != 3 {
			t.Fatalf("%d entries for pi_2, want 3", n)
		}
		check(t)
	})

	t.Run("live and test money never share an account", func(t *testing.T) {
		if _, err := post(t, pool, ledger.Charge("pi_live", merchant, true, "usd", 5000)); err != nil {
			t.Fatal(err)
		}
		if got := balance(ledger.MerchantPending(merchant, true, "usd").Code); got != -(5000 - ledger.Fee("usd", 5000)) {
			t.Fatalf("live pending = %d", got)
		}
		if got := balance(ledger.MerchantPending(merchant, false, "usd").Code); got != -1912-(1000-ledger.Fee("usd", 1000)) {
			t.Fatalf("test pending changed: %d", got)
		}
	})

	t.Run("the database rejects an unbalanced transaction at commit", func(t *testing.T) {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (id, merchant_id, livemode, currency, type, source_id)
				VALUES ('txn_bad', $1, false, 'usd', 'charge', 'pi_bad')`, merchant); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO ledger_entries (transaction_id, account_id, amount)
				SELECT 'txn_bad', id, 100 FROM ledger_accounts LIMIT 1`)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "unbalanced") {
			t.Fatalf("err = %v, want unbalanced", err)
		}
	})

	t.Run("the database rejects mixing currencies", func(t *testing.T) {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			// Create an eur account through a valid posting, then try a usd/eur transaction by hand.
			if _, err := ledger.Post(ctx, tx, ledger.Charge("pi_eur", merchant, false, "eur", 1000)); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (id, merchant_id, livemode, currency, type, source_id)
				VALUES ('txn_mix', $1, false, 'usd', 'charge', 'pi_mix')`, merchant); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO ledger_entries (transaction_id, account_id, amount)
				SELECT 'txn_mix', id, CASE WHEN currency = 'usd' THEN 100 ELSE -100 END
				FROM ledger_accounts WHERE code IN ('platform:test:usd:network_receivable', 'platform:test:eur:network_receivable')`)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "mixes currencies") {
			t.Fatalf("err = %v, want mixes currencies", err)
		}
	})

	t.Run("history is append-only", func(t *testing.T) {
		for _, stmt := range []string{
			`UPDATE ledger_entries SET amount = amount * 2`,
			`DELETE FROM ledger_entries`,
			`UPDATE ledger_transactions SET description = 'edited'`,
			`DELETE FROM ledger_transactions`,
		} {
			if _, err := pool.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
				t.Errorf("%s: err = %v, want append-only", stmt, err)
			}
		}
		check(t)
	})

	t.Run("Check reports a cached balance that drifted", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE ledger_accounts SET balance = balance + 1 WHERE code = $1`,
			ledger.PlatformFees(false, "usd").Code); err != nil {
			t.Fatal(err)
		}
		problems, err := ledger.Check(ctx, pool)
		if err != nil || len(problems) != 1 || !strings.Contains(problems[0], "platform:test:usd:fees") {
			t.Fatalf("problems = %v, %v", problems, err)
		}
	})
}
