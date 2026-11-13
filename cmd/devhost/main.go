// devhost runs a fake Marlin App Server on loopback for development.
// It stands in for the Rust host binary until the sidecar ships it; the
// flags mirror the planned host interface (docs/design.md §5.2).
//
// Usage:
//
//	go run ./cmd/devhost --bind-addr 127.0.0.1:0 --auth-token secret --session-id gui-session
//
// It prints "host ready addr=<addr>" once serving, then blocks until SIGTERM.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"marlin-desktop/fakehost"
)

func main() {
	bindAddr := flag.String("bind-addr", "127.0.0.1:0", "HTTP/2 listen address")
	authToken := flag.String("auth-token", "", "required token; empty disables auth (loopback dev only)")
	sessionID := flag.String("session-id", "gui-session", "Session ID reported by GetCapabilities")
	flag.Parse()

	ln, err := net.Listen("tcp", *bindAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	srv := fakehost.NewServer(fakehost.New(*sessionID), *authToken)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		srv.GracefulStop()
	}()

	fmt.Fprintf(os.Stdout, "host ready addr=%s\n", ln.Addr())
	if err := srv.Serve(ln); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
