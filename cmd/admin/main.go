// Command admin performs operator tasks until there is a public sign-up flow.
//
// Usage:
//
//	admin create-merchant -name NAME -email EMAIL
//	admin issue-key -merchant ID -mode test|live
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongthanhson/stripe-clone/internal/apikey"
)

const usage = "usage: admin create-merchant -name NAME -email EMAIL | admin issue-key -merchant ID -mode test|live"

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New(usage)
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := apikey.NewStore(pool)

	switch args[0] {
	case "create-merchant":
		fs := flag.NewFlagSet("create-merchant", flag.ContinueOnError)
		name := fs.String("name", "", "merchant name")
		email := fs.String("email", "", "merchant email")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" || *email == "" {
			return errors.New("-name and -email are required")
		}
		id, err := store.CreateMerchant(ctx, *name, *email)
		if err != nil {
			return err
		}
		fmt.Printf("merchant %d created\n", id)
		return nil

	case "issue-key":
		fs := flag.NewFlagSet("issue-key", flag.ContinueOnError)
		merchantID := fs.Int64("merchant", 0, "merchant id")
		modeFlag := fs.String("mode", "test", "test or live")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		mode, err := apikey.ParseMode(*modeFlag)
		if err != nil {
			return err
		}
		if *merchantID == 0 {
			return errors.New("-merchant is required")
		}
		key, err := store.Issue(ctx, *merchantID, mode)
		if err != nil {
			return err
		}
		fmt.Println(key.Secret)
		fmt.Fprintln(os.Stderr, "Store this key now; it cannot be shown again.")
		return nil

	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}
