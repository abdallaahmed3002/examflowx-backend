package main

import (
	"context"
	"examflow/backend/migrations"
	"github.com/jackc/pgx/v5"
	"log"
	"os"
	"time"
)

func main() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("DATABASE_URL required")
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
