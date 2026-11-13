package client_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"marlin-desktop/client"
	"marlin-desktop/fakehost"
)

// startFake serves a fake host on an ephemeral loopback port.
func startFake(t *testing.T, sessionID, token string) string {
	t.Helper()
	ln, err := fakehost.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := fakehost.NewServer(fakehost.New(sessionID), token)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}

func TestGetCapabilities(t *testing.T) {
	addr := startFake(t, "gui-session", "s3cret")
	conn, err := client.Dial(context.Background(), client.Config{Addr: addr, AuthToken: "s3cret"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	caps, err := conn.WaitReady(ctx, 0)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if caps.SessionID != "gui-session" {
		t.Errorf("SessionID = %q, want gui-session", caps.SessionID)
	}
	if caps.MaxRequestBytes == 0 || caps.MaxMessages == 0 || caps.MaxOutputTextByte == 0 {
		t.Errorf("limits not reported: %+v", caps)
	}
}

func TestMissingTokenRejected(t *testing.T) {
	addr := startFake(t, "gui-session", "s3cret")
	conn, err := client.Dial(context.Background(), client.Config{Addr: addr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	_, err = conn.GetCapabilities(context.Background())
	if err == nil {
		t.Fatal("expected Unauthenticated without token")
	}
	var ce *client.Error
	if !errors.As(err, &ce) {
		t.Fatalf("expected *client.Error, got %T: %v", err, err)
	}
	if ce.Code != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", ce.Code)
	}
}

func TestWrongTokenRejected(t *testing.T) {
	addr := startFake(t, "gui-session", "s3cret")
	conn, err := client.Dial(context.Background(), client.Config{Addr: addr, AuthToken: "wrong"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err = conn.GetCapabilities(context.Background()); err == nil {
		t.Fatal("expected rejection with wrong token")
	}
}

func TestNoSessionBinding(t *testing.T) {
	addr := startFake(t, "", "s3cret")
	conn, err := client.Dial(context.Background(), client.Config{Addr: addr, AuthToken: "s3cret"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	caps, err := conn.GetCapabilities(context.Background())
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	if caps.SessionID != "" {
		t.Errorf("SessionID = %q, want empty (no binding)", caps.SessionID)
	}
}

func TestHostUnavailableClassified(t *testing.T) {
	// A port that was never opened: nothing is listening.
	conn, err := client.Dial(context.Background(), client.Config{Addr: "127.0.0.1:1"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = conn.GetCapabilities(ctx)
	if err == nil {
		t.Fatal("expected error against a dead port")
	}
	kind, ok := client.AsErrKind(err)
	if !ok || kind != client.ErrHostUnavailable {
		t.Errorf("kind = %v (ok=%v), want ErrHostUnavailable", kind, ok)
	}
}

func TestWaitReadyTimesOut(t *testing.T) {
	conn, err := client.Dial(context.Background(), client.Config{Addr: "127.0.0.1:1"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := conn.WaitReady(ctx, 50*time.Millisecond); err == nil {
		t.Fatal("expected timeout against a dead host")
	}
}
