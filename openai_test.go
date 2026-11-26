package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// lmstudio is a fake OpenAI-style server: no Ollama paths, a model list, and replies streamed as server-sent events.
func lmstudio(t *testing.T, reply []string, seen *map[string]any) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"object":"list","data":[{"id":"qwen2.5-coder-3b-instruct"},{"id":"other"}]}`)
		case "/v1/chat/completions":
			if seen != nil {
				json.NewDecoder(r.Body).Decode(seen)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, piece := range reply {
				b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": piece}}}})
				fmt.Fprintf(w, "data: %s\n\n", b)
			}
			io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":42,"completion_tokens":7}}`+"\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.Error(w, `{"error":"Unexpected endpoint or method. (GET /api/tags)"}`, http.StatusOK) // LM Studio answers this way
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestOpenAIStyleServerIsFoundAndUsed(t *testing.T) {
	var sent map[string]any
	s := lmstudio(t, []string{`{"answer":`, ` "it works"`, `}`}, &sent)
	c := New(s.URL, "qwen2.5-coder-3b-instruct", 4096)
	ok, note := c.Status(context.Background())
	if !ok || note != "ready" {
		t.Fatalf("status: %v %q", ok, note)
	}
	var out struct{ Answer string }
	u, err := c.ChatJSON(context.Background(), "sys", "user", map[string]any{"type": "object"}, &out)
	if err != nil || out.Answer != "it works" {
		t.Fatalf("chat: %v %+v", err, out)
	}
	if u.PromptTokens != 42 || u.ReplyTokens != 7 {
		t.Errorf("usage from the last event: %+v", u)
	}
	rf, _ := sent["response_format"].(map[string]any)
	if sent["model"] != "qwen2.5-coder-3b-instruct" || rf["type"] != "json_schema" || sent["stream"] != true {
		t.Errorf("request: %+v", sent)
	}
}

func TestOpenAIStatusNamesTheModelsItHas(t *testing.T) {
	s := lmstudio(t, nil, nil)
	ok, note := New(s.URL, "qwen3.5:9b", 4096).Status(context.Background())
	if ok || !strings.Contains(note, "qwen2.5-coder-3b-instruct") || !strings.Contains(note, "model.name") {
		t.Errorf("a missing model should list what the server has: %v %q", ok, note)
	}
}

func TestOpenAIAddressWithV1AndAFencedReply(t *testing.T) {
	s := lmstudio(t, []string{"```json\n", `{"answer":"fenced"}`, "\n```"}, nil)
	c := New(s.URL+"/v1", "qwen2.5-coder-3b-instruct", 4096) // people often paste the address with /v1
	var out struct{ Answer string }
	if _, err := c.ChatJSON(context.Background(), "s", "u", nil, &out); err != nil || out.Answer != "fenced" {
		t.Fatalf("%v %+v", err, out)
	}
}

func TestOpenAIBlankSpaceEndsTheCall(t *testing.T) {
	s := lmstudio(t, append([]string{`{"answer":"`}, strings.Split(strings.Repeat(" ", maxBlank+5), "")...), nil)
	c := New(s.URL+"/v1", "qwen2.5-coder-3b-instruct", 4096)
	var out struct{ Answer string }
	if _, err := c.ChatJSON(context.Background(), "s", "u", nil, &out); err == nil || !strings.Contains(err.Error(), "stopped writing") {
		t.Errorf("want the blank-space guard, got %v", err)
	}
}

// The first-word limit holds for OpenAI-style servers too.
func TestOpenAIFirstWordLimit(t *testing.T) {
	old := maxFirstWord
	maxFirstWord = 300 * time.Millisecond
	defer func() { maxFirstWord = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select { // answers, then writes nothing
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	var out struct{ OK bool }
	start := time.Now()
	_, err := New(srv.URL+"/v1", "m", 4096).ChatJSON(context.Background(), "s", "u", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "wrote nothing") || time.Since(start) > time.Second {
		t.Fatalf("after %s: %v", time.Since(start), err)
	}
}
