// devhost runs a development Marlin App Server on loopback: a fake journal
// host plus a REAL model gateway when credentials are present. It stands in
// for the Rust host binary until the sidecar ships it; the flags mirror the
// planned host interface (docs/design.md §5.2).
//
// Usage:
//
//	go run ./cmd/devhost --provider kimi      # KIMI_API_KEY in env or .env.local
//	go run ./cmd/devhost --provider deepseek  # DEEPSEEK_API_KEY
//	go run ./cmd/devhost --probe              # one real gateway call, then exit
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

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func main() {
	loadDotEnv(".env.local")

	bindAddr := flag.String("bind-addr", "127.0.0.1:0", "HTTP/2 listen address")
	authToken := flag.String("auth-token", "", "required token; empty disables auth (loopback dev only)")
	sessionID := flag.String("session-id", "gui-session", "Session ID reported by GetCapabilities")
	provider := flag.String("provider", "echo", "model gateway: echo | deepseek | kimi")
	baseURL := flag.String("base-url", "", "provider base URL (defaults per provider)")
	model := flag.String("model", "", "model name (defaults per provider)")
	probe := flag.Bool("probe", false, "send one real gateway call ('ping') and exit")
	flag.Parse()

	var gateway fakehost.ChatGateway
	switch *provider {
	case "deepseek":
		base := or(*baseURL, "https://api.deepseek.com")
		gateway = newGateway(os.Getenv("DEEPSEEK_API_KEY"), base, or(*model, "deepseek-chat"),
			func(key, base, model string) fakehost.ChatGateway {
				return &fakehost.DeepSeekGateway{BaseURL: base, APIKey: key, Model: model}
			}, "DEEPSEEK_API_KEY", *provider)
	case "kimi", "anthropic":
		base := or(*baseURL, or(firstEnv("ANTHROPIC_BASE_URL"), "https://api.kimi.com/coding/"))
		m := or(*model, or(firstEnv("KIMI_MODEL", "ANTHROPIC_MODEL", "CLAUDE_CODE_MODEL"), "k3"))
		gateway = newGateway(firstEnv("KIMI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"), base, m,
			func(key, base, model string) fakehost.ChatGateway {
				return &fakehost.AnthropicGateway{BaseURL: base, APIKey: key, Model: model}
			}, "KIMI_API_KEY", *provider)
	default:
		gateway = fakehost.EchoGateway{}
	}
	h := fakehost.New(*sessionID)
	h.SetGateway(gateway)

	// --probe: one real gateway call, streamed to stdout, no gRPC server.
	if *probe {
		if err := h.Probe("请只回复两个字符：OK", func(chunk string) {
			fmt.Print(chunk)
		}); err != nil {
			log.Fatalf("probe failed: %v", err)
		}
		fmt.Println()
		return
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

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// newGateway builds the provider gateway or falls back to echo when the key
// is missing.
func newGateway(key, base, model string, build func(key, base, model string) fakehost.ChatGateway, envName, provider string) fakehost.ChatGateway {
	if key == "" {
		log.Printf("%s not set; falling back to the echo gateway", envName)
		return fakehost.EchoGateway{}
	}
	g := build(key, base, model)
	log.Printf("model gateway: %s at %s (model %s)", provider, base, model)
	return g
}
