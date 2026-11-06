package main

import (
	"context"
	"examflow/backend/internal/config"
	"github.com/jackc/pgx/v5"
	"log"
	"os"
	"strings"
	"time"
)

func main() {
	config.LoadEnv()
	email := strings.ToLower(strings.TrimSpace(os.Getenv("ADMIN_EMAIL")))
	url := os.Getenv("DATABASE_URL_UNPOOLED")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if email == "" || url == "" {
		log.Fatal("ADMIN_EMAIL and DATABASE_URL (or DATABASE_URL_UNPOOLED) required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := pgx.Connect(ctx, url)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close(context.Background())
	tx, err := db.Begin(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(448201)`); err != nil {
		log.Fatal(err)
	}
	var admins int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin'`).Scan(&admins); err != nil {
		log.Fatal(err)
	}
	if admins != 0 {
		log.Fatal("admin already exists; bootstrap disabled")
	}
	tag, err := tx.Exec(ctx, `UPDATE users SET role='admin' WHERE email=$1 AND role='teacher' AND suspended_at IS NULL`, email)
	if err != nil || tag.RowsAffected() != 1 {
		log.Fatal("registered active teacher not found")
	}
	if err = tx.Commit(ctx); err != nil {
		log.Fatal(err)
	}
	log.Print("first admin promoted; bootstrap is now disabled")
}
