// e2echeck drives one real turn against a running host over gRPC: the same
// path the GUI takes (dial → capabilities → send → watch → outcome). A dev
// utility for verifying the live stack without the window.
//
//	go run ./cmd/e2echeck --addr 127.0.0.1:59556 --token <token> --text "hi"
//	go run ./cmd/e2echeck --addr … --token … --cancel-after 2s   # Stop path
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"marlin-desktop/client"
)

func main() {
	addr := flag.String("addr", "", "host address (required)")
	token := flag.String("token", "", "auth token (required)")
	session := flag.String("session", "gui-session", "Session ID")
	text := flag.String("text", "你好，请用一句话介绍你自己", "user text")
	timeout := flag.Duration("timeout", 60*time.Second, "turn deadline")
	cancelAfter := flag.Duration("cancel-after", 0, "request cancellation this long after admission (0 = never)")
	verbose := flag.Bool("v", false, "print each text checkpoint with its arrival time")
	summaryOnly := flag.Bool("summary", false, "print the session summary and exit")
	flag.Parse()
	if *addr == "" || *token == "" {
		log.Fatal("--addr and --token are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	conn, err := client.Dial(ctx, client.Config{Addr: *addr, AuthToken: *token})
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	caps, err := conn.WaitReady(ctx, 0)
	if err != nil {
		log.Fatalf("not ready: %v", err)
	}
	fmt.Printf("caps: session=%s max_request=%d max_output=%d\n", caps.SessionID, caps.MaxRequestBytes, caps.MaxOutputTextByte)

	m := client.NewSessionManager(conn, *session)
	if *summaryOnly {
		s, err := m.Summary(ctx)
		if err != nil {
			log.Fatalf("summary: %v", err)
		}
		latest := "none"
		if s.Latest != nil {
			latest = fmt.Sprintf("ordinal=%d", s.Latest.Ordinal)
		}
		fmt.Printf("summary: latest=%s recovery=%s\n", latest, s.RecoveryState)
		return
	}
	if err := m.SyncFromSummary(ctx); err != nil {
		log.Fatalf("summary: %v", err)
	}
	turn, err := m.Send(ctx, fmt.Sprintf("e2e-%d", time.Now().UnixMilli()), *text)
	if err != nil {
		log.Fatalf("send: %v", err)
	}
	fmt.Printf("turn: ordinal=%d\n", turn.Ordinal)

	if *cancelAfter > 0 {
		go func() {
			time.Sleep(*cancelAfter)
			r, err := m.Cancel(context.Background(), turn)
			if err != nil {
				log.Printf("cancel: %v", err)
				return
			}
			fmt.Printf("cancel receipt: completed=%v requested=%v outcome=%s\n",
				r.Completed, r.CancellationRequested, r.Outcome)
		}()
	}

	events := make(chan client.SessionEvent, 64)
	followCtx, followCancel := context.WithCancel(ctx)
	defer followCancel()
	followErr := make(chan error, 1)
	go func() { followErr <- m.Follow(followCtx, turn, events) }()

	start := time.Now()
	out := make([]byte, 0, 4096)
	for {
		select {
		case ev := <-events:
			switch ev.Kind {
			case client.EventStarted:
				fmt.Printf("[%6.2fs] event: started\n", time.Since(start).Seconds())
			case client.EventText:
				if *verbose {
					fmt.Printf("[%6.2fs] text +%d bytes\n", time.Since(start).Seconds(), len(ev.Text))
				}
				out = append(out, ev.Text...)
			case client.EventOutcome:
				fmt.Printf("event: outcome=%s\n", ev.Outcome)
				fmt.Printf("text (%d bytes):\n%s\n", len(out), out)
				return
			}
		case <-ctx.Done():
			log.Fatalf("deadline: partial text %q", out)
		case err := <-followErr:
			if err != nil {
				log.Fatalf("watch: %v", err)
			}
			followErr = nil // terminal outcome is already buffered in events
		}
	}
}
