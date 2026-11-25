package gateway

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/robertjndw/oakestra-openai-api/internal/scheduler"
)

// hintHeaderPrefix marks request headers that are hints for the scheduler.
// They are canonicalised by net/http, hence the odd casing.
const hintHeaderPrefix = "X-Ooa-"

// buildRequest extracts what a strategy may want to look at. Anything the
// body does not say stays at its zero value; the engine validates the rest.
func buildRequest(api string, fields map[string]json.RawMessage, h http.Header) scheduler.Request {
	r := scheduler.Request{API: api, Raw: fields, Hints: map[string]string{}}
	_ = json.Unmarshal(fields["stream"], &r.Stream)
	r.Tools = nonEmpty(fields["tools"])

	var maxTokens, maxCompletion int
	_ = json.Unmarshal(fields["max_tokens"], &maxTokens)
	_ = json.Unmarshal(fields["max_completion_tokens"], &maxCompletion)
	r.MaxTokens = maxTokens
	if maxCompletion > 0 {
		r.MaxTokens = maxCompletion
	}

	chars := 0
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(fields["messages"], &msgs) == nil {
		for _, m := range msgs {
			text := contentText(m.Content)
			r.Messages = append(r.Messages, scheduler.Message{Role: m.Role, Content: text})
			chars += len(text)
		}
	}
	switch api {
	case "completions":
		r.Prompt = contentText(fields["prompt"])
	case "embeddings":
		r.Prompt = contentText(fields["input"])
	}
	chars += len(r.Prompt)
	// Four characters per token is the usual rule of thumb for English; it
	// is only meant to separate short from long prompts.
	r.ApproxTokens = (chars + 3) / 4

	var meta map[string]json.RawMessage
	if json.Unmarshal(fields["metadata"], &meta) == nil {
		for k, v := range meta {
			var s string
			if json.Unmarshal(v, &s) != nil {
				s = strings.TrimSpace(string(v))
			}
			r.Hints[strings.ToLower(k)] = s
		}
	}
	for k, vs := range h {
		if strings.HasPrefix(k, hintHeaderPrefix) && len(vs) > 0 {
			r.Hints[strings.ToLower(strings.TrimPrefix(k, hintHeaderPrefix))] = vs[0]
		}
	}
	return r
}

func nonEmpty(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != "[]" && s != "{}"
}

// contentText flattens an OpenAI content value: a string, an array of text
// parts, or (for prompts and embedding inputs) an array of strings. Parts
// that are not text, such as images, are skipped.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		var str string
		if json.Unmarshal(p, &str) == nil {
			texts = append(texts, str)
			continue
		}
		var part struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(p, &part) == nil && part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}
