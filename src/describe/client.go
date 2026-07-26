package describe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	sdk "github.com/cameraui/sdk/go"
)

// defaultRetryBackoff is how long Complete waits before its single retry. Short
// enough to be irrelevant against a 90-second budget, long enough to let a
// rate-limited or restarting endpoint actually recover — retrying instantly
// against a 429 just earns a second 429. Overridable as a field so tests do not
// pay for it.
const defaultRetryBackoff = 3 * time.Second

// maxErrorBodyBytes caps how much of a failed response is read into the error
// message. Endpoints behind a reverse proxy answer with HTML error pages, and
// without a limit a single failure would put a multi-kilobyte log line into the
// plugin's log for every event.
const maxErrorBodyBytes = 2048

// errFormatUnsupported marks a 400 that specifically complained about
// response_format, so Complete can retry without the field instead of writing the
// endpoint off as broken. Several llama.cpp-derived OpenAI-compatible servers
// reject it outright, and parseDescription is lenient enough not to need it.
var errFormatUnsupported = errors.New("describe: endpoint rejected response_format")

// Client calls an OpenAI-compatible /chat/completions endpoint and parses the
// reply into an sdk.EventDescription.
//
// Stateless apart from its http.Client and retry delay, so one Client is shared
// by the whole plugin and is safe for concurrent use — every per-request input
// (endpoint, key, model) arrives in the Config passed to Complete, which is what
// lets settings changes take effect without rebuilding anything.
type Client struct {
	httpClient *http.Client

	// retryBackoff is a field rather than a constant use so tests can collapse
	// it to a millisecond; production always gets defaultRetryBackoff.
	retryBackoff time.Duration
}

// NewClient returns a Client backed by an http.Client with no timeout of its own.
//
// That is deliberate: every call's deadline comes from the context the Describer
// derives from aiTimeoutSeconds, so there is exactly one place a timeout is
// configured and no way for a second, invisible one to cut a request short. The
// http.Client is shared across calls so its connection pool is reused — a
// per-call client would open a fresh TLS connection for every event.
func NewClient() *Client {
	return &Client{
		httpClient:   &http.Client{},
		retryBackoff: defaultRetryBackoff,
	}
}

// Complete sends frames plus contextText to cfg's model and returns the parsed
// description. frames are JPEG bytes in chronological order, oldest first.
//
// Retry policy: exactly one retry, after retryBackoff, and only for conditions
// that can plausibly succeed a moment later — a transport error, a timeout, HTTP
// 429, or HTTP 5xx. This is not belt-and-braces. Ollama cold-loads a model into
// VRAM on the first request after a restart and routinely blows the deadline
// doing it, so without the retry the first event after every restart would be
// silently undescribed. Anything else (401, 404, a model name that doesn't
// exist, an unparseable reply) fails immediately, because retrying it only burns
// a second inference to reach the same answer.
//
// The response_format fallback is checked BEFORE the transient classification and
// is not counted as the retry: it is a capability probe, not a failure, so a
// server that rejects JSON mode still gets a genuine attempt.
func (c *Client) Complete(ctx context.Context, cfg Config, contextText string, frames [][]byte) (sdk.EventDescription, error) {
	desc, err := c.attempt(ctx, cfg, contextText, frames, true)
	if err == nil {
		return desc, nil
	}

	// A server that refuses response_format gets one attempt without it rather
	// than being treated as broken. Must precede the isTransient check: a 400 is
	// not transient, so the order below would return it unretried.
	if errors.Is(err, errFormatUnsupported) {
		return c.attempt(ctx, cfg, contextText, frames, false)
	}

	if !isTransient(err) {
		return sdk.EventDescription{}, err
	}

	// Waiting on ctx as well as the timer means a cancelled or expired deadline
	// ends the call now instead of sleeping out the backoff first — the whole
	// point of the deadline is to bound how long the serial worker is held.
	select {
	case <-ctx.Done():
		return sdk.EventDescription{}, ctx.Err()
	case <-time.After(c.retryBackoff):
	}

	return c.attempt(ctx, cfg, contextText, frames, true)
}

// attempt performs one full request/parse cycle. withResponseFormat controls
// whether the request asks for JSON mode, and is threaded through to
// classifyStatus so a complaint about the field is only believed when the field
// was actually sent.
func (c *Client) attempt(ctx context.Context, cfg Config, contextText string, frames [][]byte, withResponseFormat bool) (sdk.EventDescription, error) {
	payload, err := json.Marshal(buildRequest(cfg, contextText, frames, withResponseFormat))
	if err != nil {
		return sdk.EventDescription{}, fmt.Errorf("describe: marshal request: %w", err)
	}

	// Config.BaseURL has already had any trailing slash stripped by Load, so
	// this concatenation cannot produce a double slash (which some gateways
	// 404 on).
	url := cfg.BaseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return sdk.EventDescription{}, fmt.Errorf("describe: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Only set when a key exists: a local Ollama or llama.cpp server needs none,
	// and an empty `Bearer ` value is worse than an absent header — some servers
	// reject it outright rather than ignoring it.
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport failures — connection refused, DNS, a tripped deadline —
		// are exactly the retryable class: the endpoint may simply not be up
		// yet, which is the normal state of a local runtime that starts after
		// the NVR does.
		return sdk.EventDescription{}, transientError{err: fmt.Errorf("describe: post %s: %w", url, err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return sdk.EventDescription{}, classifyStatus(resp.StatusCode, string(snippet), withResponseFormat)
	}

	// Only the first choice's content is read. n is never set above the default
	// of 1, so additional choices cannot occur; ignoring usage/finish_reason is
	// deliberate — nothing here acts on them, and decoding into a narrow
	// anonymous struct means an endpoint adding fields cannot break parsing.
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return sdk.EventDescription{}, fmt.Errorf("describe: decode response: %w", err)
	}
	// A 200 with no choices is a malformed reply, not an empty result. Not
	// transient: a server doing this will keep doing it.
	if len(parsed.Choices) == 0 {
		return sdk.EventDescription{}, errors.New("describe: response contained no choices")
	}

	return parseDescription(parsed.Choices[0].Message.Content)
}

// classifyStatus turns a non-200 into one of the three outcomes Complete
// distinguishes: errFormatUnsupported (drop the field and retry), a
// transientError (worth one retry), or a plain permanent error.
//
// The response_format check is a substring match on the body because there is no
// standard error code for it — each server words the complaint differently, but
// all of them name the field. It is gated on sentResponseFormat so a 400 whose
// body merely happens to mention the field cannot send Complete down the
// fallback path when the field was never in the request.
func classifyStatus(status int, body string, sentResponseFormat bool) error {
	if status == http.StatusBadRequest && sentResponseFormat && strings.Contains(strings.ToLower(body), "response_format") {
		return errFormatUnsupported
	}
	err := fmt.Errorf("describe: endpoint returned %d: %s", status, strings.TrimSpace(body))
	// 429 means "later"; 5xx means the server is unhealthy right now. Every
	// other 4xx is a statement about the request itself and will not change.
	if status == http.StatusTooManyRequests || status >= 500 {
		return transientError{err: err}
	}
	return err
}

// transientError marks a failure worth exactly one retry. A wrapper type rather
// than a sentinel because each occurrence carries its own message; Unwrap keeps
// errors.Is/As working through it for callers that want the underlying cause.
type transientError struct{ err error }

func (t transientError) Error() string { return t.err.Error() }
func (t transientError) Unwrap() error { return t.err }

// isTransient reports whether err was classified as retryable anywhere in its
// chain.
func isTransient(err error) bool {
	var t transientError
	return errors.As(err, &t)
}

// chatRequest and friends are the wire types for the chat-completions request.
//
// Content is `any` rather than a concrete type so the system message can be a
// plain string while the user message is a parts array: OpenAI accepts either
// form for system, but several compatible servers only handle the string form,
// and there is no reason to send an array where a string will do.
//
// ResponseFormat is a pointer with omitempty so the field disappears entirely
// from the payload on the fallback attempt — a server that rejects the field
// would reject `"response_format": null` just as readily.
type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type contentPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *imageURLRef `json:"image_url,omitempty"`
}

type imageURLRef struct {
	URL string `json:"url"`
}

// buildRequest assembles the chat-completions payload: a string system message
// carrying systemPrompt, then a user message whose first part is the detector
// context and whose remaining parts are the frames as base64 JPEG data URIs, in
// chronological order.
//
// Data URIs rather than hosted URLs because the frames only exist inside this
// process — they were extracted to a temp directory that is already gone by the
// time this runs — and because a self-hosted NVR has no public URL to serve them
// from. Frame order is preserved exactly as given: it is the only thing telling
// the model what happened before what.
func buildRequest(cfg Config, contextText string, frames [][]byte, withResponseFormat bool) chatRequest {
	parts := make([]contentPart, 0, len(frames)+1)
	parts = append(parts, contentPart{Type: "text", Text: contextText})
	for _, frame := range frames {
		parts = append(parts, contentPart{
			Type:     "image_url",
			ImageURL: &imageURLRef{URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(frame)},
		})
	}

	req := chatRequest{
		Model: cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: parts},
		},
	}
	if withResponseFormat {
		req.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	return req
}

// parseDescription extracts an EventDescription from a model reply.
//
// Deliberately lenient about the envelope and strict about the contents.
//
// Lenient, because servers that ignore response_format — which is most local
// runtimes — wrap the JSON in code fences or bracket it with prose, and refusing
// those replies would throw away perfectly good descriptions over presentation.
// So the outermost { ... } is located and decoded, and anything around it is
// discarded.
//
// Strict, because a reply with neither a title nor a description is treated as a
// FAILURE rather than a success with blank fields: persisting one would light up
// the frontend's description badge with nothing behind it, which reads to a user
// as a broken UI rather than a failed inference. A blank summary is tolerated —
// it degrades a notification, it does not misrepresent anything.
func parseDescription(content string) (sdk.EventDescription, error) {
	body := stripCodeFences(strings.TrimSpace(content))

	// First "{" to last "}" spans the object even when it contains nested
	// objects, and drops any surrounding prose. A reply containing two separate
	// objects would produce garbage here, but no observed model does that while
	// answering this prompt, and the json.Unmarshal below rejects the result
	// rather than inventing one.
	start := strings.Index(body, "{")
	end := strings.LastIndex(body, "}")
	if start < 0 || end <= start {
		return sdk.EventDescription{}, fmt.Errorf("describe: no JSON object in model reply: %q", truncate(content, 200))
	}

	var desc sdk.EventDescription
	if err := json.Unmarshal([]byte(body[start:end+1]), &desc); err != nil {
		return sdk.EventDescription{}, fmt.Errorf("describe: decode model reply: %w", err)
	}

	// Clamped, not rejected: the frontend maps this onto a fixed three-state
	// badge, so an out-of-range value has no rendering, but a model that copies
	// a 1-10 scale out of its training data has still produced a usable
	// narrative worth keeping.
	if desc.ThreatLevel < 0 {
		desc.ThreatLevel = 0
	}
	if desc.ThreatLevel > 2 {
		desc.ThreatLevel = 2
	}

	desc.Title = strings.TrimSpace(desc.Title)
	desc.Description = strings.TrimSpace(desc.Description)
	desc.Summary = strings.TrimSpace(desc.Summary)

	if desc.Title == "" && desc.Description == "" {
		return sdk.EventDescription{}, errors.New("describe: model reply had no title or description")
	}
	return desc, nil
}

// stripCodeFences removes a leading ```lang line and the matching trailing ```
// line — the shape a chatty model wraps JSON in even when told not to.
//
// Only applied when the reply actually starts with a fence, so prose that merely
// contains a fence somewhere is left for the brace scan in parseDescription to
// handle.
func stripCodeFences(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop through the end of the opening fence line, which may carry a language
	// tag ("```json").
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	}
	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// truncate bounds a model reply quoted into an error message, so an endpoint
// answering with an essay cannot flood the log. Cuts on bytes rather than runes,
// which can split a multi-byte character — acceptable in a diagnostic string,
// and cheaper than decoding.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
