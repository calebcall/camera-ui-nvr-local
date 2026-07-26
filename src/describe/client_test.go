package describe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testConfig is the minimum Config a request needs. It deliberately skips
// Enabled, FrameCount, Labels, and MinConfidence: those gate whether generation
// happens at all (Describer's job), and Client never reads them.
func testConfig(baseURL string) Config {
	return Config{
		BaseURL: baseURL,
		Model:   "test-model",
		Timeout: 5 * time.Second,
	}
}

// respondJSON writes a well-formed chat-completions envelope whose single
// choice's message content is content, so each test below varies only the model's
// reply text and not the transport shape around it.
func respondJSON(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
	})
}

// TestComplete_HappyPath_SendsTheExpectedRequestShape is the wire-contract test.
// Every assertion here is something a real OpenAI-compatible server enforces, so
// this is what stands in for the integration test the feature cannot have: the
// path, the content type, JSON mode, a plain-string system message (an array
// there is rejected by several llama.cpp-based servers), and one text part
// followed by the frames as base64 JPEG data URIs.
func TestComplete_HappyPath_SendsTheExpectedRequestShape(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		respondJSON(w, `{"title":"Person at door","description":"A person approaches.","summary":"Someone approached.","threatLevel":1}`)
	}))
	defer srv.Close()

	got, err := NewClient().Complete(context.Background(), testConfig(srv.URL), "context text", [][]byte{[]byte("frame-a"), []byte("frame-b")})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.Title != "Person at door" || got.ThreatLevel != 1 {
		t.Errorf("description = %+v", got)
	}

	if body["model"] != "test-model" {
		t.Errorf("model = %v, want test-model", body["model"])
	}
	if rf, ok := body["response_format"].(map[string]any); !ok || rf["type"] != "json_object" {
		t.Errorf("response_format = %v, want {type: json_object}", body["response_format"])
	}

	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages = %v, want 2", body["messages"])
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" {
		t.Errorf("messages[0].role = %v, want system", sys["role"])
	}
	if _, isString := sys["content"].(string); !isString {
		t.Errorf("system content = %T, want a plain string for maximum server compatibility", sys["content"])
	}

	user := msgs[1].(map[string]any)
	if user["role"] != "user" {
		t.Errorf("messages[1].role = %v, want user", user["role"])
	}
	parts, ok := user["content"].([]any)
	if !ok || len(parts) != 3 {
		t.Fatalf("user content = %v, want 1 text part + 2 image parts", user["content"])
	}
	if parts[0].(map[string]any)["type"] != "text" {
		t.Errorf("parts[0].type = %v, want text", parts[0].(map[string]any)["type"])
	}
	for i := 1; i < 3; i++ {
		p := parts[i].(map[string]any)
		if p["type"] != "image_url" {
			t.Errorf("parts[%d].type = %v, want image_url", i, p["type"])
		}
		url := p["image_url"].(map[string]any)["url"].(string)
		if !strings.HasPrefix(url, "data:image/jpeg;base64,") {
			t.Errorf("parts[%d] url = %q, want a jpeg data URI", i, url)
		}
	}
}

// TestComplete_SendsAuthorizationOnlyWhenKeyIsSet covers both halves of the
// bring-your-own-endpoint promise. An empty key is a legitimate configuration
// (local Ollama needs none) and sending `Authorization: Bearer ` for it makes
// some servers reject the request, so the header must be absent rather than
// blank.
func TestComplete_SendsAuthorizationOnlyWhenKeyIsSet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		key     string
		wantHdr string
	}{
		{"with key", "sk-abc", "Bearer sk-abc"},
		{"without key", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("Authorization")
				respondJSON(w, `{"title":"T","description":"D"}`)
			}))
			defer srv.Close()

			cfg := testConfig(srv.URL)
			cfg.APIKey = tc.key
			if _, err := NewClient().Complete(context.Background(), cfg, "ctx", [][]byte{[]byte("f")}); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if got != tc.wantHdr {
				t.Errorf("Authorization = %q, want %q", got, tc.wantHdr)
			}
		})
	}
}

// TestComplete_WrappedOrFencedJSON_IsStillParsed is why the parser hunts for the
// outermost braces instead of unmarshalling the reply directly. Servers that
// ignore response_format — most local runtimes — routinely return exactly these
// three shapes, and rejecting them would throw away perfectly good descriptions.
func TestComplete_WrappedOrFencedJSON_IsStillParsed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"code fenced", "```json\n{\"title\":\"T\",\"description\":\"D\"}\n```"},
		{"bare fenced", "```\n{\"title\":\"T\",\"description\":\"D\"}\n```"},
		{"prose wrapped", "Sure! Here you go:\n{\"title\":\"T\",\"description\":\"D\"}\nHope that helps."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				respondJSON(w, tc.content)
			}))
			defer srv.Close()

			got, err := NewClient().Complete(context.Background(), testConfig(srv.URL), "ctx", [][]byte{[]byte("f")})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if got.Title != "T" || got.Description != "D" {
				t.Errorf("description = %+v", got)
			}
		})
	}
}

// TestComplete_ThreatLevelOutOfRange_IsClamped protects the frontend, which maps
// this integer onto a fixed three-state badge. A model that invents 7 (or copies
// a 1-10 scale from its training data) must not produce a value no consumer has
// a rendering for.
func TestComplete_ThreatLevelOutOfRange_IsClamped(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  int
		want int
	}{
		{"above range", 7, 2},
		{"below range", -3, 0},
		{"in range", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				respondJSON(w, `{"title":"T","description":"D","threatLevel":`+strconv.Itoa(tc.raw)+`}`)
			}))
			defer srv.Close()

			got, err := NewClient().Complete(context.Background(), testConfig(srv.URL), "ctx", [][]byte{[]byte("f")})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if got.ThreatLevel != tc.want {
				t.Errorf("ThreatLevel = %d, want %d", got.ThreatLevel, tc.want)
			}
		})
	}
}

// TestComplete_UnusableReply_IsAnError is the strict half of the parser's
// contract. A refusal, a truncated reply, or an object with nothing in the two
// fields that matter must fail loudly rather than persist a blank description —
// storing one lights up the frontend's badge with nothing behind it, which reads
// as a bug in the UI rather than a failed inference.
func TestComplete_UnusableReply_IsAnError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"no json at all", "I'm sorry, I can't help with that."},
		{"malformed json", `{"title": "T", "description":`},
		{"blank title and description", `{"title":"","description":"","summary":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				respondJSON(w, tc.content)
			}))
			defer srv.Close()

			if _, err := NewClient().Complete(context.Background(), testConfig(srv.URL), "ctx", [][]byte{[]byte("f")}); err == nil {
				t.Error("Complete() = nil error, want a failure")
			}
		})
	}
}

// TestComplete_TransientStatus_IsRetriedOnce pins the retry policy in both
// directions, and the call counts are the real assertions. 429 and 5xx can
// succeed a moment later (a rate limit expiring, Ollama finishing a cold model
// load), so they are worth one more inference. 401 and 404 cannot — the key is
// wrong or the path is wrong — so retrying them only doubles the damage.
func TestComplete_TransientStatus_IsRetriedOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		wantCalls int32
		wantErr   bool
	}{
		{"429 then success", http.StatusTooManyRequests, 2, false},
		{"500 then success", http.StatusInternalServerError, 2, false},
		{"401 is not retried", http.StatusUnauthorized, 1, true},
		{"404 is not retried", http.StatusNotFound, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&calls, 1) == 1 {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"error":"nope"}`))
					return
				}
				respondJSON(w, `{"title":"T","description":"D"}`)
			}))
			defer srv.Close()

			c := NewClient()
			c.retryBackoff = time.Millisecond // keep the test fast
			_, err := c.Complete(context.Background(), testConfig(srv.URL), "ctx", [][]byte{[]byte("f")})

			if tc.wantErr && err == nil {
				t.Error("Complete() = nil error, want a failure")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Complete() = %v, want success on retry", err)
			}
			if got := atomic.LoadInt32(&calls); got != tc.wantCalls {
				t.Errorf("server calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

// TestComplete_PersistentFailure_GivesUpAfterOneRetry bounds the retry: exactly
// one, never a loop. A permanently broken endpoint must cost two requests per
// event, not an unbounded stream of them.
func TestComplete_PersistentFailure_GivesUpAfterOneRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient()
	c.retryBackoff = time.Millisecond
	if _, err := c.Complete(context.Background(), testConfig(srv.URL), "ctx", [][]byte{[]byte("f")}); err == nil {
		t.Error("Complete() = nil error, want a failure")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server calls = %d, want exactly 2 (one attempt, one retry)", got)
	}
}

// TestComplete_ResponseFormatRejected_RetriesWithoutIt covers the one
// compatibility fallback in the client. Several llama.cpp-derived servers 400 on
// response_format outright; since the parser tolerates fenced and prose-wrapped
// JSON anyway, dropping the field is strictly better than reporting the endpoint
// as broken. The presence-per-call assertion is what proves the fallback ran
// instead of the generic transient retry, which would have re-sent the field.
func TestComplete_ResponseFormatRejected_RetriesWithoutIt(t *testing.T) {
	var mu sync.Mutex
	var sawResponseFormat []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		_, present := body["response_format"]

		mu.Lock()
		sawResponseFormat = append(sawResponseFormat, present)
		mu.Unlock()

		if present {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"response_format is not supported"}}`))
			return
		}
		respondJSON(w, `{"title":"T","description":"D"}`)
	}))
	defer srv.Close()

	c := NewClient()
	c.retryBackoff = time.Millisecond
	got, err := c.Complete(context.Background(), testConfig(srv.URL), "ctx", [][]byte{[]byte("f")})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.Title != "T" {
		t.Errorf("description = %+v", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sawResponseFormat) != 2 || !sawResponseFormat[0] || sawResponseFormat[1] {
		t.Errorf("response_format presence per call = %v, want [true false]", sawResponseFormat)
	}
}

// TestComplete_ContextCancelled_ReturnsAnError proves the deadline the Describer
// builds from aiTimeoutSeconds actually terminates a call. Without this, a
// wedged endpoint would hold the serial worker forever and silently stop every
// later event from being described.
func TestComplete_ContextCancelled_ReturnsAnError(t *testing.T) {
	// The handler must never answer, but it must also be releasable: waiting
	// only on r.Context() hangs httptest.Server.Close, because a client-side
	// cancellation does not reliably propagate to the server's request context.
	// unblock is closed before Close runs (defers are LIFO), so the handler
	// always returns.
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-unblock:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(unblock)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := NewClient().Complete(ctx, testConfig(srv.URL), "ctx", [][]byte{[]byte("f")}); err == nil {
		t.Error("Complete() = nil error, want a context deadline failure")
	}
}
