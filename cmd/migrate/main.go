package main

import (
	"context"
	"examflow/backend/internal/config"
	"examflow/backend/migrations"
	"github.com/jackc/pgx/v5"
	"log"
	"os"
	"time"
)

func main() {
	config.LoadEnv()
	// Migrations require a direct, unpooled connection to Neon Postgres.
	url := os.Getenv("DATABASE_URL_UNPOOLED")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		log.Fatal("DATABASE_URL or DATABASE_URL_UNPOOLED required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close(context.Background())
	if err := migrations.ValidateVersionNames(); err != nil {
		log.Fatal(err)
	}
	if err := migrations.Apply(ctx, conn); err != nil {
		log.Fatal(err)
	}
	log.Print("migrations applied")
}
