// Package wa holds the WhatsApp code shared by the API server and the bot:
// login, number handling, profile lookup, reactions and the contact flow.
package wa

import (
	"context"
	"fmt"
	"os"

	_ "github.com/mattn/go-sqlite3" // SQLite driver for the session store
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// NewClient opens (or creates) the session file and returns a client. Both
// programs use the same file so one QR scan logs in either of them.
func NewClient(ctx context.Context, dbPath string) (*whatsmeow.Client, error) {
	container, err := sqlstore.New(ctx, "sqlite3", "file:"+dbPath+"?_foreign_keys=on", waLog.Noop)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("get device: %w", err)
	}
	return whatsmeow.NewClient(device, waLog.Stdout("Client", "WARN", true)), nil
}

// Connect shows a QR code on first run, otherwise just connects.
func Connect(ctx context.Context, client *whatsmeow.Client) error {
	if client.Store.ID != nil {
		return client.Connect()
	}
	qrChan, err := client.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := client.Connect(); err != nil {
		return err
	}
	for evt := range qrChan {
		if evt.Event == "code" {
			fmt.Println("Scan this QR code in WhatsApp > Linked devices:")
			qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
		} else {
			fmt.Println("Login event:", evt.Event)
		}
	}
	return nil
}
