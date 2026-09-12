// Command shortlinkctl is the operator CLI. It currently issues API keys.
//
// It shares the storage and service packages with the server rather than
// talking to the database on its own, so the hashing scheme can never drift
// between the tool that writes keys and the server that verifies them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"shortlink/internal/config"
	"shortlink/internal/service"
	"shortlink/internal/storage/postgres"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() string {
	return `shortlinkctl - operator CLI for the shortlink service

usage:
  shortlinkctl issue-key -name <name> [-days N]
      Issue an API key and print it once. Without -days the key never expires,
      which is what an operator key normally wants; with it, the key stops
      working after N days, the same way keys bought through the Telegram bot
      stop working when the buyer's access runs out.

The database is read from POSTGRES_DSN, the same variable the server uses.`
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Println(usage())
		return errors.New("no command given")
	}

	switch args[0] {
	case "issue-key":
		return issueKey(args[1:])
	case "help", "-h", "--help":
		fmt.Println(usage())
		return nil
	default:
		fmt.Println(usage())
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func issueKey(args []string) error {
	fs := flag.NewFlagSet("issue-key", flag.ContinueOnError)
	name := fs.String("name", "", "a label for the key, e.g. the team or app that will use it")
	days := fs.Int("days", 0, "days until the key expires; 0 means it never does")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		fs.Usage()
		return errors.New("-name is required")
	}
	if *days < 0 {
		return errors.New("-days cannot be negative")
	}

	// nil rather than a far-future date: "no expiry" is a real state of a key,
	// and writing a date would make every operator key look like a subscription
	// that happens to end in the year 3000.
	var expiresAt *time.Time
	if *days > 0 {
		at := time.Now().Add(time.Duration(*days) * 24 * time.Hour).UTC()
		expiresAt = &at
	}

	// Only the database settings: the CLI writes a key row and never hashes an
	// IP or serves a redirect.
	db, err := config.LoadDatabase()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if err := postgres.Migrate(ctx, db.DSN, log); err != nil {
		return err
	}

	pool, err := postgres.NewPool(ctx, db.DSN, 2)
	if err != nil {
		return err
	}
	defer pool.Close()

	secret, key, err := service.NewAuthenticator(postgres.NewAPIKeyRepository(pool)).Issue(ctx, *name, expiresAt)
	if err != nil {
		return fmt.Errorf("issue api key: %w", err)
	}

	// The secret goes to stdout on its own line so it can be piped into a
	// secret store; everything else is commentary on stderr.
	expiry := "never expires"
	if key.ExpiresAt != nil {
		expiry = "expires " + key.ExpiresAt.Format(time.RFC3339)
	}

	fmt.Fprintf(os.Stderr, "issued api key %q (id %d) at %s, %s\n",
		key.Name, key.ID, key.CreatedAt.Format(time.RFC3339), expiry)
	fmt.Fprintln(os.Stderr, "store it now: only its hash is kept, so it cannot be shown again.")
	fmt.Println(secret)
	return nil
}
