// Command jobkeys creates, lists and revokes API keys (ADR 013).
// It talks to Postgres directly, so whoever runs it needs database access.
// There is no API endpoint for keys on purpose.
//
//	jobkeys create -name billing-service -role client
//	jobkeys list
//	jobkeys revoke -name billing-service
//
// DATABASE_URL must be set. A new key is printed once and never stored; only
// its SHA-256 hash goes into the database.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/jackc/pgx/v5"

	"github.com/skifast/durable-job-engine/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: jobkeys create|list|revoke (see the comment at the top of main.go)")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL must be set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("create", flag.ExitOnError)
		name := fs.String("name", "", "the caller's name (becomes client_id, lease_owner or decided_by)")
		role := fs.String("role", "", "client, worker or operator")
		fs.Parse(args[1:])
		if *name == "" || *role == "" {
			return fmt.Errorf("create needs -name and -role")
		}
		// 32 random bytes (256 bits), shown as 64 hex characters. A key this long cannot be guessed.
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		key := hex.EncodeToString(raw)
		_, err := conn.Exec(ctx, `INSERT INTO api_keys (name, role, key_hash) VALUES ($1, $2, $3)`,
			*name, *role, store.HashKey(key))
		if err != nil {
			return err // for example: the name is taken, or the role is not one of the three
		}
		fmt.Printf("Created %s key %q.\nKey (shown once, not stored): %s\n", *role, *name, key)

	case "list":
		rows, err := conn.Query(ctx, `SELECT name, role, created_at, revoked_at FROM api_keys ORDER BY created_at`)
		if err != nil {
			return err
		}
		defer rows.Close()
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tROLE\tCREATED\tREVOKED")
		for rows.Next() {
			var name, role string
			var created store.Time
			var revoked *store.Time
			if err := rows.Scan(&name, &role, &created, &revoked); err != nil {
				return err
			}
			rev := "-"
			if revoked != nil {
				rev = revoked.Std().Format("2006-01-02 15:04:05Z07:00")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, role, created.Std().Format("2006-01-02 15:04:05Z07:00"), rev)
		}
		tw.Flush()
		return rows.Err()

	case "revoke":
		fs := flag.NewFlagSet("revoke", flag.ExitOnError)
		name := fs.String("name", "", "the key's name")
		fs.Parse(args[1:])
		if *name == "" {
			return fmt.Errorf("revoke needs -name")
		}
		tag, err := conn.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE name = $1 AND revoked_at IS NULL`, *name)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("no active key named %q", *name)
		}
		fmt.Printf("Revoked %q. It stops working on the next request.\n", *name)

	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	return nil
}
