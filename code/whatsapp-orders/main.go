// Command whatsapp-orders reads incoming WhatsApp messages, filters them, has Claude
// turn customers' messages into orders, posts each order to your system's webhook,
// and can ask customers to confirm their order. See README.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "config.yaml", "your config file")
	pairPhone := flag.String("pair-phone", "", "link WhatsApp with a code instead of a QR code: your number with country code, like 6281234567890")
	try := flag.String("try", "", "test only: read this text as if a customer sent it, print the order Claude finds, and exit (one message per line)")
	verbose := flag.Bool("v", false, "also log why messages are skipped (never their text)")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reader := NewExtractor(cfg)
	if *try != "" {
		if err := tryOrder(ctx, cfg, reader, *try); err != nil {
			log.Fatal(err)
		}
		return
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(cfg.DataDir, "orders.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	deliverer := NewDeliverer(cfg.Output, store, filepath.Join(cfg.DataDir, "orders.jsonl"))
	go deliverer.Run(ctx)

	replier := NewReplier(cfg.Reply, store)
	pipeline := NewPipeline(ctx, cfg, store, reader, deliverer, replier, *verbose)
	if err := pipeline.Resume(); err != nil {
		log.Fatal(err)
	}
	wa, err := ConnectWhatsApp(ctx, cfg.DataDir, *pairPhone, pipeline.Handle)
	if err != nil {
		log.Fatal(err)
	}
	defer wa.Disconnect()
	go replier.Run(ctx, wa.Send)

	if cfg.Output.WebhookURL == "" {
		log.Printf("Test mode: orders are only written to %s. Set output.webhook_url to send them to your system.",
			filepath.Join(cfg.DataDir, "orders.jsonl"))
	} else {
		log.Printf("Orders go to %s.", cfg.Output.WebhookURL)
	}
	switch cfg.Reply.Mode {
	case "preview":
		log.Println("Confirmation messages are only shown in this log (reply.mode is preview).")
	case "send":
		log.Println("Customers who order in a private chat get a confirmation message.")
	}
	log.Println("Running. Press Ctrl+C to stop.")
	select {
	case <-ctx.Done():
	case <-wa.LoggedOut():
	}
}

// tryOrder reads text typed on the command line as if a customer had sent it, so you
// can check your product list, confirmation message and API key before linking WhatsApp.
func tryOrder(ctx context.Context, cfg Config, reader *Extractor, text string) error {
	now := time.Now()
	var batch []Incoming
	for i, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			batch = append(batch, Incoming{ID: fmt.Sprint(i), ChatID: "try", SenderName: "Test customer", Time: now, Text: line})
		}
	}
	if len(batch) == 0 {
		return errors.New("-try needs some text")
	}
	reading, err := reader.Extract(ctx, batch, nil)
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(reading.Order, "", "  ")
	if err != nil {
		return err
	}
	fmt.Printf("%s\n(read by %s)\n", out, reading.Model)
	if cfg.Reply.Mode != "off" && reading.Order.IsOrder && len(reading.Order.Items) > 0 {
		fmt.Printf("\nThe confirmation message would be:\n%s\n", confirmText(cfg.Reply, reading.Order, "Test customer"))
	}
	return nil
}
