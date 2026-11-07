// Command api serves POST /flow: look up a WhatsApp contact and react to a
// message, driven by HTTP instead of chat commands.
//
// Run from the repo root so it shares session.db with the bot:
//
//	go run ./cmd/api
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"wabot/wa"
)

const dbPath = "session.db"

func run(ctx context.Context) error {
	client, err := wa.NewClient(ctx, dbPath)
	if err != nil {
		return err
	}
	if err := wa.Connect(ctx, client); err != nil {
		return err
	}
	defer client.Disconnect()
	slog.Info("connected", "jid", client.Store.ID)

	stopAPI, err := startAPI(client)
	if err != nil {
		return err
	}
	defer stopAPI()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	return nil
}

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
