package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Model servers speak one of two protocols. Ollama has its own (/api/tags, /api/chat). LM Studio, llama.cpp's server,
// vLLM, Jan and most others speak OpenAI's (/v1/models, /v1/chat/completions). The client finds out which by asking,
// once, unless CODIST_MODEL_API says (ollama or openai).
const (
	apiOllama = "ollama"
	apiOpenAI = "openai"
)

var apiSeen sync.Map // URL -> protocol found, shared by the clients of one server

// api is the protocol the server at c.URL speaks: CODIST_MODEL_API if set, OpenAI's when the address ends in /v1,
// else what Status found, else Ollama's. Nothing is asked here, so an Ollama server sees exactly the requests it
// always did; Status looks for an OpenAI-style server only when Ollama's question fails.
func (c *Client) api(_ context.Context) string {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("CODIST_MODEL_API"))); v == apiOllama || v == apiOpenAI {
		return v
	}
	if strings.HasSuffix(c.URL, "/v1") {
		return apiOpenAI
	}
	if v, ok := apiSeen.Load(c.URL); ok {
		return v.(string)
	}
	return apiOllama
}

// tryOpenAI asks whether the server speaks OpenAI's protocol, and remembers it when it does.
func (c *Client) tryOpenAI(ctx context.Context) bool {
	if os.Getenv("CODIST_MODEL_API") == apiOllama {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if ok, _ := getJSON(ctx, c.openaiBase()+"/v1/models", &struct {
		Data []any `json:"data"`
	}{}); !ok {
		return false
	}
	apiSeen.Store(c.URL, apiOpenAI)
	logger().Info("model server speaks the OpenAI protocol", "url", c.URL)
	return true
}

// getJSON reads a JSON reply; ok only for a 200 that decodes.
func getJSON(ctx context.Context, url string, out any) (bool, int) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, resp.StatusCode
	}
	return json.NewDecoder(resp.Body).Decode(out) == nil, resp.StatusCode
}

// openaiBase is the server address without a trailing /v1, which people often include.
func (c *Client) openaiBase() string { return strings.TrimSuffix(c.URL, "/v1") }

// statusOpenAI says whether an OpenAI-style server answers and has the model.
func (c *Client) statusOpenAI(ctx context.Context) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if ok, code := getJSON(ctx, c.openaiBase()+"/v1/models", &models); !ok {
		if code == 0 {
			return false, "no model server at " + c.URL + "; start it or set model.url in .codist/config.lua"
		}
		return false, fmt.Sprintf("the server at %s answered %d to /v1/models", c.URL, code)
	}
	var names []string
	for _, m := range models.Data {
		if m.ID == c.Name || strings.TrimSuffix(m.ID, ":latest") == c.Name {
			return true, "ready"
		}
		names = append(names, m.ID)
	}
	if len(names) == 0 {
		return false, "the server at " + c.URL + " has no model loaded; load " + c.Name + " in it"
	}
	return false, fmt.Sprintf("%s is not on the server at %s; it has: %s. Set model.name to one of them", c.Name, c.URL, strings.Join(names[:min(5, len(names))], ", "))
}

// chatOpenAI is ChatJSON for an OpenAI-style server: the same request, the reply streamed as server-sent events, and
// the same guards against a reply that turns into blank space or goes silent.
func (c *Client) chatOpenAI(ctx context.Context, system, user string, schema any, out any) (Usage, error) {
	req := map[string]any{
		"model": c.Name, "stream": true, "temperature": 0.1, "max_tokens": 2500,
		"messages":       []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": user}},
		"stream_options": map[string]any{"include_usage": true},
	}
	if schema != nil {
		req["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "answer", "schema": schema}}
	} else {
		req["response_format"] = map[string]any{"type": "json_object"}
	}
	body, _ := json.Marshal(req)
	start := time.Now()
	call := Call{ID: nextID(), Label: LabelOf(ctx), Model: c.Name, System: system, User: user, Start: start}
	if Observer != nil {
		Observer(call)
	}
	var content strings.Builder
	var u Usage
	var err error
	defer func() {
		if Observer != nil {
			call.Done, call.Reply, call.Usage = true, content.String(), u
			call.Usage.Took = time.Since(start)
			if err != nil {
				call.Err = err.Error()
			}
			Observer(call)
		}
	}()
	ctx, stop := context.WithTimeout(ctx, 12*time.Minute)
	defer stop()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.openaiBase()+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Usage{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream")
	var waited atomic.Bool // the first word did not come in time
	first := time.AfterFunc(maxFirstWord, func() { waited.Store(true); stop() })
	defer first.Stop()
	resp, err := c.HTTP.Do(hr)
	if err != nil {
		if werr := waitError(waited.Load(), false, c.Name, ""); werr != nil {
			err = werr
		} else {
			err = fmt.Errorf("model server: %w", err)
		}
		return Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		logger().Warn("model server refused", "model", c.Name, "status", resp.Status, "body", fmt.Sprintf("%.300s", raw))
		err = fmt.Errorf("model server said %s: %s", resp.Status, strings.TrimSpace(string(raw)))
		return Usage{}, err
	}
	var stalled atomic.Bool
	idle := time.AfterFunc(time.Hour, func() { stalled.Store(true); stop() })
	idle.Stop() // armed by the first words
	defer idle.Stop()
	call.Phase = "reading"
	blank, pieces := 0, 0
	shown := time.Now()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var part struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &part) != nil {
			continue
		}
		if part.Error != nil {
			err = fmt.Errorf("model server: %s", part.Error.Message)
			return Usage{}, err
		}
		if part.Usage != nil {
			u.PromptTokens, u.ReplyTokens = part.Usage.Prompt, part.Usage.Completion
		}
		text := ""
		if len(part.Choices) > 0 {
			text = part.Choices[0].Delta.Content
		}
		if text == "" {
			continue
		}
		content.WriteString(text)
		if pieces++; pieces == 1 {
			call.Phase, call.FirstToken = "writing", time.Since(start)
		}
		if Observer != nil && time.Since(shown) > 400*time.Millisecond {
			shown = time.Now()
			call.Reply, call.Usage.ReplyTokens, call.Usage.Took = content.String(), pieces, time.Since(start)
			Observer(call)
		}
		if strings.TrimSpace(text) == "" {
			if blank++; blank >= maxBlank {
				stop()
				logger().Warn("model wrote only blank space; call ended", "model", c.Name, "reply", fmt.Sprintf("%.200s", strings.TrimSpace(content.String())))
				err = fmt.Errorf("the model stopped writing words after: %.200s", strings.TrimSpace(content.String()))
				return Usage{}, err
			}
		} else {
			blank = 0
			first.Stop()
			idle.Reset(maxSilence)
		}
	}
	if serr := sc.Err(); serr != nil {
		if stalled.Load() || waited.Load() {
			err = waitError(waited.Load(), stalled.Load(), c.Name, content.String())
		} else {
			err = fmt.Errorf("model server reply: %w", serr)
		}
		return Usage{}, err
	}
	u.Took = time.Since(start)
	if u.ReplyTokens == 0 {
		u.ReplyTokens = pieces
	}
	text := strings.TrimSpace(content.String())
	// Some servers wrap JSON in a code fence even when asked for JSON.
	text = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(text, "```json"), "```"), "```")
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(text)), out); jerr != nil {
		logger().Warn("model reply not JSON", "model", c.Name, "reply", fmt.Sprintf("%.300s", text))
		err = fmt.Errorf("the model did not return valid JSON: %.200s", text)
		return u, err
	}
	return u, nil
}
