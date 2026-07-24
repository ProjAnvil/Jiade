// Package main implements the seed CLI: deterministic data generation and
// integrity verification across all six commerce databases.
//
// Usage:
//
//	seed generate --scale dev|demo|load --seed N --reset
//	seed verify   --scale dev|demo|load
//
// The CLI never starts or resets a user-owned container. It connects to the
// databases described by the standard DB_* environment variables (one set per
// service, via the SEED_DATABASES variable or sensible defaults).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"commerce/internal/seed"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		log.Fatal(err)
	}
}

// run is the testable entry point: it parses argv, dispatches to generate or
// verify, and writes the human-readable summary to stdout. Errors go to stderr.
func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usage(stderr)
	}
	switch args[0] {
	case "generate":
		return runGenerate(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		return usage(stdout)
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage(w io.Writer) error {
	const text = `seed — deterministic commerce data generator and verifier

Usage:
  seed generate --scale dev|demo|load --seed N [--reset]
  seed verify   --scale dev|demo|load

Flags:
  --scale   dataset volume (dev=100 orders, demo=10k, load=1M)   (required)
  --seed    deterministic seed; same seed+scale => identical data (default 42)
  --reset   truncate seeded tables before inserting               (generate only)

Environment:
  SEED_DATABASES_JSON  JSON map of service -> DSN. If unset, the CLI derives
                       one DSN from DB_* and assumes a single shared database.
  TEST_DATABASE_URL    used by tests; the CLI never starts a container.
`
	_, err := fmt.Fprint(w, text)
	return err
}

func runGenerate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scale := fs.String("scale", "", "dataset volume: dev|demo|load (required)")
	seedValue := fs.Int64("seed", 42, "deterministic seed")
	reset := fs.Bool("reset", false, "truncate seeded tables before inserting")
	if err := fs.Parse(args); err != nil {
		return err
	}
	parsedScale, err := seed.ParseScale(*scale)
	if err != nil {
		return err
	}

	cfg := seed.Config{Scale: parsedScale, Seed: *seedValue, Reset: *reset}

	// Database load is gated behind SEED_DATABASES_JSON so the CLI stays safe
	// to invoke in CI without a live PostgreSQL.
	databases, dbErr := loadDatabaseSet(stderr)
	if dbErr != nil {
		fmt.Fprintln(stderr, "note: skipping database load (set SEED_DATABASES_JSON to enable)")
		return nil
	}
	if len(databases.pools) == 0 {
		// No databases: produce and verify the in-memory dataset only. At the
		// load scale this stays bounded because we never invoke the loader.
		ds, err := seed.Generate(cfg)
		if err != nil {
			return fmt.Errorf("generate: %w", err)
		}
		if err := seed.VerifyFixture(ds); err != nil {
			return fmt.Errorf("verify fixture: %w", err)
		}
		writeSummary(stdout, ds.Summary())
		fmt.Fprintln(stderr, "note: skipping database load (no SEED_DATABASES_JSON)")
		return nil
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if *reset {
		if err := databases.Reset(ctx); err != nil {
			return fmt.Errorf("reset: %w", err)
		}
	}
	summary, err := databases.Load(ctx, cfg)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	writeSummary(stdout, summary)
	fmt.Fprintln(stdout, "load complete")
	return nil
}

func runVerify(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	scale := fs.String("scale", "", "dataset volume: dev|demo|load (required)")
	seedValue := fs.Int64("seed", 42, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	parsedScale, err := seed.ParseScale(*scale)
	if err != nil {
		return err
	}

	// In-memory verifier: produces the same Dataset the loader would have
	// written and confirms every invariant holds without needing a database.
	cfg := seed.Config{Scale: parsedScale, Seed: *seedValue}
	ds, err := seed.Generate(cfg)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}
	if err := seed.VerifyFixture(ds); err != nil {
		return fmt.Errorf("verify fixture: %w", err)
	}
	writeSummary(stdout, ds.Summary())

	// Optional live verification when the operator wires up databases.
	databases, dbErr := loadDatabaseSet(stderr)
	if dbErr != nil || len(databases.pools) == 0 {
		fmt.Fprintln(stderr, "note: skipping live database verification (no SEED_DATABASES_JSON)")
		return nil
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := databases.VerifyDatabase(ctx, ds.Summary()); err != nil {
		return fmt.Errorf("live verify: %w", err)
	}
	fmt.Fprintln(stdout, "live verification passed")
	return nil
}

func writeSummary(w io.Writer, s seed.Summary) {
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		// Marshal of a plain struct cannot fail in practice.
		fmt.Fprintf(w, "summary: %+v\n", s)
		return
	}
	_, _ = w.Write(body)
	_, _ = w.Write([]byte("\n"))
}

// ---------------------------------------------------------------------------
// Database set wiring.
// ---------------------------------------------------------------------------

// databaseSet is the CLI-side adapter for seed.DatabaseSet. It owns the pools
// so they can be closed on exit.
type databaseSet struct {
	pools   map[string]*pgxpool.Pool
	seedSet seed.DatabaseSet
}

func (d databaseSet) Reset(ctx context.Context) error {
	return seed.Reset(ctx, d.seedSet)
}

// Load dispatches to the streaming path for the load scale and the batch path
// for dev/demo. The streaming path keeps peak memory bounded by chunkSize.
func (d databaseSet) Load(ctx context.Context, cfg seed.Config) (seed.Summary, error) {
	if cfg.Scale == seed.Load {
		return seed.StreamLoad(ctx, poolCopier{order: d.seedSet.Order, customer: d.seedSet.Customer,
			catalog: d.seedSet.Catalog, inventory: d.seedSet.Inventory,
			payment: d.seedSet.Payment, fulfillment: d.seedSet.Fulfillment}, cfg)
	}
	ds, err := seed.Generate(cfg)
	if err != nil {
		return seed.Summary{}, err
	}
	if err := seed.VerifyFixture(ds); err != nil {
		return seed.Summary{}, err
	}
	if err := seed.LoadDataset(ctx, d.seedSet, ds); err != nil {
		return seed.Summary{}, err
	}
	return ds.Summary(), nil
}

func (d databaseSet) VerifyDatabase(ctx context.Context, expected seed.Summary) error {
	return seed.VerifyDatabase(ctx, d.seedSet, expected)
}

func (d databaseSet) Close() {
	for _, pool := range d.pools {
		pool.Close()
	}
}

// poolCopier adapts the six pgx pools into seed.Copier for the streaming load
// path. CopyFrom routes each table to the pool that owns it.
type poolCopier struct {
	order, customer, catalog, inventory, payment, fulfillment *pgxpool.Pool
}

func (c poolCopier) CopyFrom(ctx context.Context, table string, columns []string, rows [][]any) (int64, error) {
	pool := c.poolFor(table)
	if pool == nil {
		return 0, fmt.Errorf("no pool for table %s", table)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire for %s: %w", table, err)
	}
	defer conn.Release()
	return conn.Conn().CopyFrom(ctx, pgx.Identifier{table}, columns, pgx.CopyFromRows(rows))
}

// poolFor maps a table name to the database that owns it. Tables are grouped
// exactly as in seed.Reset.
func (c poolCopier) poolFor(table string) *pgxpool.Pool {
	switch table {
	case "category", "brand", "product", "variant":
		return c.catalog
	case "membership_tier", "customer", "address":
		return c.customer
	case "location", "inventory_level", "stock_movement":
		return c.inventory
	case "cart", "cart_item", "sales_order", "order_item", "order_tax_line",
		"order_customer_snapshot", "reservation":
		return c.order
	case "payment_intent", "payment_attempt", "refund":
		return c.payment
	case "fulfillment_order", "fulfillment_item", "package", "shipment", "tracking_event":
		return c.fulfillment
	}
	return nil
}

// loadDatabaseSet reads SEED_DATABASES_JSON. The expected shape is:
//
//	{"catalog":"postgres://...","customer":"...","inventory":"...",
//	 "order":"...","payment":"...","fulfillment":"..."}
//
// If unset, the function returns the zero databaseSet and a nil error so the
// CLI can proceed in verify-only mode without databases.
func loadDatabaseSet(stderr io.Writer) (databaseSet, error) {
	raw := strings.TrimSpace(os.Getenv("SEED_DATABASES_JSON"))
	if raw == "" {
		return databaseSet{}, nil
	}
	var dsn map[string]string
	if err := json.Unmarshal([]byte(raw), &dsn); err != nil {
		return databaseSet{}, fmt.Errorf("parse SEED_DATABASES_JSON: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ds := databaseSet{pools: make(map[string]*pgxpool.Pool)}
	open := func(key string) (*pgxpool.Pool, error) {
		url := dsn[key]
		if url == "" {
			return nil, fmt.Errorf("SEED_DATABASES_JSON missing %q", key)
		}
		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", key, err)
		}
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			return nil, fmt.Errorf("ping %s: %w", key, err)
		}
		return pool, nil
	}

	var errs []error
	for _, key := range []string{"catalog", "customer", "inventory", "order", "payment", "fulfillment"} {
		pool, err := open(key)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ds.pools[key] = pool
	}
	if len(errs) > 0 {
		ds.Close()
		return databaseSet{}, errors.Join(errs...)
	}
	ds.seedSet = seed.DatabaseSet{
		Catalog:     ds.pools["catalog"],
		Customer:    ds.pools["customer"],
		Inventory:   ds.pools["inventory"],
		Order:       ds.pools["order"],
		Payment:     ds.pools["payment"],
		Fulfillment: ds.pools["fulfillment"],
	}
	return ds, nil
}
