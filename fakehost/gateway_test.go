package fakehost_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	appv1 "marlin-desktop/gen/marlin/app/v1"
	"marlin-desktop/client"
	"marlin-desktop/fakehost"
)

// sseServer serves an OpenAI-style streaming endpoint and records requests.
type sseServer struct {
	*httptest.Server
	mu       sync.Mutex
	lastBody string
	status   int
	chunks   []string
}

func newSSEServer(t *testing.T, chunks ...string) *sseServer {
	t.Helper()
	s := &sseServer{status: http.StatusOK, chunks: chunks}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.lastBody = string(readAll(r))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(s.status)
		if s.status != http.StatusOK {
			return
		}
		for _, c := range s.chunks {
			payload, _ := jsonDelta(c)
			w.Write([]byte("data: " + payload + "\n\n"))
		}
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(s.Close)
	return s
}

func jsonDelta(content string) (string, error) {
	type delta struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	d := delta{}
	d.Choices = append(d.Choices, struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	}{})
	d.Choices[0].Delta.Content = content
	b, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func readAll(r *http.Request) []byte {
	body := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	return body
}

// startGatewayHost serves a fake host wired to a test SSE endpoint.
func startGatewayHost(t *testing.T, sse *sseServer, apiKey string) (string, *fakehost.Host) {
	t.Helper()
	ln, err := fakehost.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	h := fakehost.New("gui-session")
	h.SetGateway(&fakehost.DeepSeekGateway{BaseURL: sse.URL, APIKey: apiKey, Model: "test-model"})
	srv := fakehost.NewServer(h, "")
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String(), h
}

// dialManager connects one SessionManager (the watermark must survive
// across turns, as in the real app).
func dialManager(t *testing.T, addr string) *client.SessionManager {
	t.Helper()
	conn, err := client.Dial(context.Background(), client.Config{Addr: addr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return client.NewSessionManager(conn, "gui-session")
}

// sendFollow sends one turn and collects its events until the outcome.
func sendFollow(t *testing.T, m *client.SessionManager, subID, text string) (client.TurnRef, []client.SessionEvent) {
	t.Helper()
	turn, err := m.Send(context.Background(), subID, text)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	ch := make(chan client.SessionEvent, 64)
	go func() { _ = m.Follow(context.Background(), turn, ch) }()
	var evs []client.SessionEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			evs = append(evs, ev)
			if ev.Kind == client.EventOutcome {
				return turn, evs
			}
		case <-deadline:
			t.Fatal("timed out waiting for outcome")
		}
	}
}

func TestDeepSeekGatewayStreams(t *testing.T) {
	sse := newSSEServer(t, "Hello", " from", " the provider")
	addr, _ := startGatewayHost(t, sse, "test-key")
	m := dialManager(t, addr)

	_, evs := sendFollow(t, m, "send-1", "hi")
	var text strings.Builder
	for _, ev := range evs {
		if ev.Kind == client.EventText {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "Hello from the provider" {
		t.Errorf("streamed text = %q", text.String())
	}
	if evs[len(evs)-1].Outcome != appv1.SessionTurnOutcome_SESSION_TURN_OUTCOME_COMPLETED {
		t.Errorf("outcome = %v, want COMPLETED", evs[len(evs)-1].Outcome)
	}
	// The gateway received the API key and the user message.
	sse.mu.Lock()
	defer sse.mu.Unlock()
	if !strings.Contains(sse.lastBody, `"hi"`) {
		t.Errorf("request body missing user message: %s", sse.lastBody)
	}
}

func TestHistoryCarriesPreviousTurns(t *testing.T) {
	sse := newSSEServer(t, "ok")
	addr, h := startGatewayHost(t, sse, "test-key")
	m := dialManager(t, addr)

	_, evs1 := sendFollow(t, m, "send-1", "first question")
	if evs1[len(evs1)-1].Outcome != appv1.SessionTurnOutcome_SESSION_TURN_OUTCOME_COMPLETED {
		t.Fatalf("first turn outcome = %v", evs1[len(evs1)-1].Outcome)
	}
	// Second turn: the gateway must receive turn 1 as user+assistant history.
	_, _ = sendFollow(t, m, "send-2", "second question")

	sse.mu.Lock()
	defer sse.mu.Unlock()
	body := sse.lastBody
	for _, want := range []string{`"content":"first question"`, `"content":"ok"`, `"content":"second question"`} {
		if !strings.Contains(body, want) {
			t.Errorf("history missing %s in %s", want, body)
		}
	}
	if h == nil {
		t.Error("host handle nil")
	}
}

func TestGatewayFailureBeforeTextIsRefusal(t *testing.T) {
	sse := newSSEServer(t) // no chunks; will return 401
	sse.mu.Lock()
	sse.status = http.StatusUnauthorized
	sse.mu.Unlock()
	addr, _ := startGatewayHost(t, sse, "wrong-key")
	m := dialManager(t, addr)

	_, evs := sendFollow(t, m, "send-1", "hi")
	if evs[len(evs)-1].Outcome != appv1.SessionTurnOutcome_SESSION_TURN_OUTCOME_REJECTED_BEFORE_DISPATCH {
		t.Errorf("outcome = %v, want REJECTED_BEFORE_DISPATCH", evs[len(evs)-1].Outcome)
	}
	for _, ev := range evs {
		if ev.Kind == client.EventText && ev.Text != "" {
			t.Errorf("refusal must carry no text, got %q", ev.Text)
		}
	}
}

func TestEchoGatewayFallback(t *testing.T) {
	ln, err := fakehost.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	h := fakehost.New("gui-session") // default gateway = echo
	srv := fakehost.NewServer(h, "")
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	m := dialManager(t, ln.Addr().String())
	_, evs := sendFollow(t, m, "send-1", "say something")
	var text strings.Builder
	for _, ev := range evs {
		if ev.Kind == client.EventText {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "say something" {
		t.Errorf("echo text = %q", text.String())
	}
}
