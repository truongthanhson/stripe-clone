package migrations_test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/truongthanhson/stripe-clone/migrations"
)

func TestMigrationsUpDownUp(t *testing.T) {
	ctx := context.Background()

	ctr, err := postgres.Run(ctx, "postgres:17",
		postgres.WithDatabase("stripe"),
		postgres.WithUsername("stripe"),
		postgres.WithPassword("stripe"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO merchants (name, email) VALUES ('Acme', 'ops@acme.test')`); err != nil {
		t.Fatalf("insert after up: %v", err)
	}
	if _, err := provider.DownTo(ctx, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
	if v, err := provider.GetDBVersion(ctx); err != nil || v != 0 {
		t.Fatalf("version after down = %d, %v; want 0", v, err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
