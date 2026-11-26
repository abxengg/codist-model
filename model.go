// Package model talks to the model server the user runs. Codist does not contain a model: the user
// starts one (Ollama, or a server that speaks OpenAI's protocol such as LM Studio or llama.cpp) and gives Codist its
// address.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// maxBlank is how many pieces of nothing but blank space in a row end a reply. JSON between fields has a few.
const maxBlank = 60

// maxSilence is how long a model that has started writing may go without sending a word. A model that has broken
// its own JSON text (an unescaped quote) can go on using up its whole reply limit without sending anything: seen
// with Qwen 3.5, minutes of silence each time.
var maxSilence = 30 * time.Second

// Client calls one model on one server.
type Client struct {
	URL, Name string
	Ctx       int
	HTTP      *http.Client
}

// New makes a client. A review can take minutes on a laptop, so the timeout is long.
func New(url, name string, ctx int) *Client {
	return &Client{URL: strings.TrimRight(url, "/"), Name: name, Ctx: ctx, HTTP: &http.Client{Timeout: 15 * time.Minute}}
}

// Status says whether the server answers and has the model, in words a person can act on.
func (c *Client) Status(ctx context.Context) (bool, string) {
	if c.api(ctx) == apiOpenAI {
		return c.statusOpenAI(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/api/tags", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger().Debug("model server not reachable", "url", c.URL, "err", err)
		return false, "no model server at " + c.URL + "; start Ollama or set model.url in .codist/config.lua"
	}
	defer resp.Body.Close()
	var tags struct {
		Models []struct{ Name string } `json:"models"`
		Error  string                  `json:"error"` // LM Studio answers Ollama's question with an error, as JSON
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil || resp.StatusCode != http.StatusOK || tags.Error != "" {
		// Not Ollama: maybe a server that speaks OpenAI's protocol (LM Studio, llama.cpp, vLLM).
		if c.tryOpenAI(ctx) {
			return c.statusOpenAI(ctx)
		}
		logger().Warn("model server answered unlike Ollama", "url", c.URL, "status", resp.Status, "err", err)
		return false, "the server at " + c.URL + " did not answer like Ollama or like an OpenAI-style server (LM Studio, llama.cpp)"
	}
	for _, m := range tags.Models {
		if m.Name == c.Name || m.Name == c.Name+":latest" {
			return true, "ready"
		}
	}
	logger().Debug("model not on server", "url", c.URL, "model", c.Name, "models", len(tags.Models))
	return false, c.Name + " is not on the server; run: ollama pull " + c.Name
}

// Usage is what one call cost, as the model server measured it: reading the prompt (prefill) sets the time to
// the first token, writing the reply (decode) the time per token after it.
type Usage struct {
	PromptTokens, ReplyTokens int
	Took                      time.Duration // the whole call, as Codist saw it
	Prefill, Decode, Load     time.Duration // as the server measured them; zero when it does not say
}

// DecodeRate is reply tokens per second, or 0 when unknown.
func (u Usage) DecodeRate() float64 {
	if u.Decode <= 0 {
		return 0
	}
	return float64(u.ReplyTokens) / u.Decode.Seconds()
}

// ChatJSON sends a system and user message and decodes the JSON reply into out. schema, when
// given, is a JSON schema the server must follow, so required fields such as a quoted line of code
// cannot be left out.
func (c *Client) ChatJSON(ctx context.Context, system, user string, schema any, out any) (Usage, error) {
	if c.api(ctx) == apiOpenAI {
		return c.chatOpenAI(ctx, system, user, schema, out)
	}
	format := any("json")
	if schema != nil {
		format = schema
	}
	body, _ := json.Marshal(map[string]any{
		// think false: a model that reasons before it answers (Qwen 3.5, MiMo) can spend the whole reply on
		// reasoning and return no JSON at all. Models without that mode take the field and ignore it.
		// stream true: the reply is read as it is written, so a reply that turns into blank space is cut off (below).
		"model": c.Name, "stream": true, "format": format, "keep_alive": "30m", "think": false,
		// A reply cut off at the limit is not JSON and the whole call is lost; Qwen 3.5 writes replies of 1,500 tokens.
		"options":  map[string]any{"temperature": 0.1, "num_ctx": c.Ctx, "num_predict": 2500},
		"messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": user}},
	})
	start := time.Now()
	call := Call{ID: nextID(), Label: LabelOf(ctx), Model: c.Name, System: system, User: user, Start: start}
	if Observer != nil {
		Observer(call)
	}
	var reply struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		PromptEvalCount    int   `json:"prompt_eval_count"`
		EvalCount          int   `json:"eval_count"`
		PromptEvalDuration int64 `json:"prompt_eval_duration"` // nanoseconds
		EvalDuration       int64 `json:"eval_duration"`
		LoadDuration       int64 `json:"load_duration"`
	}
	var u Usage
	var err error
	defer func() {
		if Observer != nil {
			call.Done, call.Reply, call.Usage = true, reply.Message.Content, u
			call.Usage.Took = time.Since(start)
			if err != nil {
				call.Err = err.Error()
			}
			Observer(call)
		}
	}()
	logger().Debug("model call", "model", c.Name, "system_chars", len(system), "user_chars", len(user), "schema", schema != nil)
	ctx, stop := context.WithTimeout(ctx, 12*time.Minute) // no reply is worth longer; stop also ends a blank one
	defer stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		logger().Warn("model call failed", "model", c.Name, "took", time.Since(start), "err", err)
		err = fmt.Errorf("model server: %w", err)
		return Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		logger().Warn("model server refused", "model", c.Name, "status", resp.Status, "body", fmt.Sprintf("%.300s", raw))
		err = fmt.Errorf("model server said %s: %s", resp.Status, strings.TrimSpace(string(raw)))
		return Usage{}, err
	}
	// The reply comes in pieces, each a JSON object; the last carries the counts. A model that types a double quote
	// inside a text ends that text early, and the reply format then lets it write nothing but blank space, which it
	// does until the limit: minutes lost, and Ollama's MLX runner has hung on it. So a long run of blank pieces
	// ends the call.
	var content strings.Builder
	blank, pieces := 0, 0
	var stalled atomic.Bool
	idle := time.AfterFunc(time.Hour, func() { stalled.Store(true); stop() })
	idle.Stop() // armed by the first words
	defer idle.Stop()
	call.Phase = "reading"
	shown := time.Now()
	for dec := json.NewDecoder(resp.Body); ; {
		var part struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Error              string `json:"error"`
			PromptEvalCount    int    `json:"prompt_eval_count"`
			EvalCount          int    `json:"eval_count"`
			PromptEvalDuration int64  `json:"prompt_eval_duration"`
			EvalDuration       int64  `json:"eval_duration"`
			LoadDuration       int64  `json:"load_duration"`
		}
		if derr := dec.Decode(&part); derr == io.EOF {
			break
		} else if derr != nil && stalled.Load() {
			reply.Message.Content = content.String()
			logger().Warn("model went silent; call ended", "model", c.Name, "after", maxSilence, "reply", fmt.Sprintf("%.200s", strings.TrimSpace(reply.Message.Content)))
			err = fmt.Errorf("the model stopped writing words after: %.200s", strings.TrimSpace(reply.Message.Content))
			return Usage{}, err
		} else if derr != nil {
			logger().Warn("model server reply unreadable", "model", c.Name, "err", derr)
			err = fmt.Errorf("model server reply: %w", derr)
			return Usage{}, err
		}
		if part.Error != "" {
			err = fmt.Errorf("model server: %s", part.Error)
			return Usage{}, err
		}
		content.WriteString(part.Message.Content)
		if part.Message.Content != "" {
			if pieces++; pieces == 1 {
				call.Phase, call.FirstToken = "writing", time.Since(start)
			}
			if Observer != nil && time.Since(shown) > 400*time.Millisecond { // a watcher sees the reply grow
				shown = time.Now()
				call.Reply, call.Usage.ReplyTokens, call.Usage.Took = content.String(), pieces, time.Since(start)
				Observer(call)
			}
		}
		switch {
		case part.Message.Content == "":
		case strings.TrimSpace(part.Message.Content) == "":
			blank++
		default:
			blank = 0
			idle.Reset(maxSilence)
		}
		if blank >= maxBlank {
			stop()
			reply.Message.Content = content.String()
			logger().Warn("model wrote only blank space; call ended", "model", c.Name, "took", time.Since(start).Round(time.Millisecond), "reply", fmt.Sprintf("%.200s", strings.TrimSpace(reply.Message.Content)))
			err = fmt.Errorf("the model stopped writing words after: %.200s", strings.TrimSpace(reply.Message.Content))
			return Usage{}, err
		}
		if part.EvalCount > 0 || part.PromptEvalCount > 0 {
			reply.PromptEvalCount, reply.EvalCount = part.PromptEvalCount, part.EvalCount
			reply.PromptEvalDuration, reply.EvalDuration, reply.LoadDuration = part.PromptEvalDuration, part.EvalDuration, part.LoadDuration
		}
	}
	reply.Message.Content = content.String()
	u = Usage{PromptTokens: reply.PromptEvalCount, ReplyTokens: reply.EvalCount, Took: time.Since(start),
		Prefill: time.Duration(reply.PromptEvalDuration), Decode: time.Duration(reply.EvalDuration), Load: time.Duration(reply.LoadDuration)}
	logger().Debug("model replied", "model", c.Name, "prompt_tokens", u.PromptTokens, "reply_tokens", u.ReplyTokens,
		"took", u.Took.Round(time.Millisecond), "prefill", u.Prefill.Round(time.Millisecond), "tok_per_s", int(u.DecodeRate()))
	if jerr := json.Unmarshal([]byte(reply.Message.Content), out); jerr != nil {
		logger().Warn("model reply not JSON", "model", c.Name, "reply", fmt.Sprintf("%.300s", reply.Message.Content))
		err = fmt.Errorf("the model did not return valid JSON: %.200s", reply.Message.Content)
		return u, err
	}
	return u, nil
}
