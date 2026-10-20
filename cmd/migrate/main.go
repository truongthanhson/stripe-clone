// Command migrate applies the embedded schema migrations.
//
// Usage: migrate up|down|status
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/truongthanhson/stripe-clone/migrations"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(context.Background(), os.Args[1:]); err != nil {
		logger.Error("migrate failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: migrate up|down|status")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return err
	}

	switch args[0] {
	case "up":
		results, err := provider.Up(ctx)
		printResults(results)
		return err
	case "down":
		result, err := provider.Down(ctx)
		if result != nil {
			printResults([]*goose.MigrationResult{result})
		}
		return err
	case "status":
		statuses, err := provider.Status(ctx)
		if err != nil {
			return err
		}
		for _, s := range statuses {
			fmt.Printf("%-8s %s\n", s.State, s.Source.Path)
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q: usage: migrate up|down|status", args[0])
	}
}

func printResults(results []*goose.MigrationResult) {
	if len(results) == 0 {
		fmt.Println("no migrations to run")
	}
	for _, r := range results {
		fmt.Println(r)
	}
}
