// Command warpsocks exposes several SOCKS5 exits (typically WARP tunnels) as a
// single SOCKS5 proxy that rotates the exit per TCP connection.
//
// Point cmd/serve at it and each captcha solve starts from a different egress
// address, so a solve that upstream rate-limited on one IP is retried on
// another:
//
//	go run ./cmd/warpsocks -exits 127.0.0.1:4001,127.0.0.1:4002,127.0.0.1:4003
//	go run ./cmd/serve -auto -proxy socks5://127.0.0.1:1080
//
// This does not create the tunnels; run a WARP client (usque, warp-plus) per
// exit and list their SOCKS5 listen addresses.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"nvidia-playgroud-go/internal/warpsocks"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:1080", "SOCKS5 listen address for the rotating front")
	exitsFlag := flag.String("exits", "", "comma-separated SOCKS5 exits, e.g. 127.0.0.1:4001,127.0.0.1:4002 (falls back to WARPSOCKS_EXITS)")
	verbose := flag.Bool("v", false, "log every connection")
	flag.Parse()

	spec := strings.TrimSpace(*exitsFlag)
	if spec == "" {
		spec = strings.TrimSpace(os.Getenv("WARPSOCKS_EXITS"))
	}
	if spec == "" {
		log.Fatal("warpsocks: no exits; pass -exits or set WARPSOCKS_EXITS")
	}
	exits, err := warpsocks.ParseExitList(spec)
	if err != nil {
		log.Fatalf("warpsocks: %v", err)
	}
	pool, err := warpsocks.NewPool(exits)
	if err != nil {
		log.Fatalf("warpsocks: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	srv := &warpsocks.Server{Addr: *addr, Pool: pool}
	if *verbose {
		srv.Logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	log.Printf("warpsocks: %d exits %v", len(exits), exits)
	if err := srv.ListenAndServe(ctx); err != nil {
		log.Fatalf("warpsocks: %v", err)
	}
	// Give in-flight solves a moment to drain before the pool goes away.
	time.Sleep(100 * time.Millisecond)
}
