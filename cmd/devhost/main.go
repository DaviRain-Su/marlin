// devhost runs a development Marlin App Server on loopback: a fake journal
// host plus a REAL model gateway when credentials are present. It stands in
// for the Rust host binary until the sidecar ships it; the flags mirror the
// planned host interface (docs/design.md §5.2).
//
// Usage:
//
//	go run ./cmd/devhost --provider deepseek   # DEEPSEEK_API_KEY in env
//	go run ./cmd/devhost                       # no key: echo fallback
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
	"strings"
	"syscall"

	"marlin-desktop/fakehost"
)

// loadDotEnv sets KEY=VALUE pairs from path for variables not already in the
// environment. The file is git-ignored (.env.local); secrets never go into
// argv or the repo.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if os.Getenv(k) == "" {
			os.Setenv(k, strings.TrimSpace(v))
		}
	}
}

func main() {
	loadDotEnv(".env.local")
	bindAddr := flag.String("bind-addr", "127.0.0.1:0", "HTTP/2 listen address")
	authToken := flag.String("auth-token", "", "required token; empty disables auth (loopback dev only)")
	sessionID := flag.String("session-id", "gui-session", "Session ID reported by GetCapabilities")
	provider := flag.String("provider", "echo", "model gateway: echo | deepseek")
	baseURL := flag.String("base-url", "https://api.deepseek.com", "OpenAI-compatible chat endpoint base URL")
	model := flag.String("model", "deepseek-chat", "model name for the deepseek provider")
	flag.Parse()

	h := fakehost.New(*sessionID)
	switch *provider {
	case "deepseek":
		key := os.Getenv("DEEPSEEK_API_KEY")
		if key == "" {
			log.Printf("DEEPSEEK_API_KEY not set; falling back to the echo gateway")
			h.SetGateway(fakehost.EchoGateway{})
		} else {
			h.SetGateway(&fakehost.DeepSeekGateway{BaseURL: *baseURL, APIKey: key, Model: *model})
			log.Printf("model gateway: %s at %s (model %s)", *provider, *baseURL, *model)
		}
	default:
		h.SetGateway(fakehost.EchoGateway{})
	}

	ln, err := net.Listen("tcp", *bindAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	srv := fakehost.NewServer(h, *authToken)

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
