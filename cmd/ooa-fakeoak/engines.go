package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/oakestra/oakestratest"
)

// engines starts a fake OpenAI engine for every deployed instance with a
// host port, listening on bind:<host port>, and stops it when the instance
// goes away.
type engines struct {
	bind string
	log  *slog.Logger

	mu      sync.Mutex
	running map[string]func()
}

func newEngines(bind string, log *slog.Logger) *engines {
	return &engines{bind: bind, log: log, running: make(map[string]func())}
}

func engineKey(d oakestratest.Deployment) string {
	return d.ServiceID + "/" + strconv.Itoa(d.InstanceNumber)
}

// start is the fake's OnDeploy. A port that is already taken marks the
// instance FAILED, which is what a real container that cannot bind its
// port looks like to the server.
func (e *engines) start(_ context.Context, d oakestratest.Deployment) error {
	if d.HostPort == 0 {
		return nil
	}
	addr := net.JoinHostPort(e.bind, strconv.Itoa(d.HostPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		e.log.Warn("fake engine failed to start", "service", d.ServiceName, "addr", addr, "err", err)
		return err
	}
	r := newResponder(d)
	srv := &http.Server{Handler: r, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			e.log.Warn("fake engine stopped with error", "addr", addr, "err", err)
		}
	}()
	e.mu.Lock()
	// Close, not Shutdown: a removed node's containers die with open
	// connections too, and the gateway must cope with that.
	e.running[engineKey(d)] = func() { _ = srv.Close() }
	e.mu.Unlock()
	e.log.Info("fake engine started", "service", d.ServiceName, "instance", d.InstanceNumber,
		"node", d.Node.CandidateName, "addr", addr, "model", r.served, "api_key", r.key != "")
	return nil
}

// stop is the fake's OnUndeploy.
func (e *engines) stop(d oakestratest.Deployment) {
	e.mu.Lock()
	stop, ok := e.running[engineKey(d)]
	delete(e.running, engineKey(d))
	e.mu.Unlock()
	if ok {
		stop()
		e.log.Info("fake engine stopped", "service", d.ServiceName, "instance", d.InstanceNumber)
	}
}

func (e *engines) closeAll() {
	e.mu.Lock()
	stops := make([]func(), 0, len(e.running))
	for k, s := range e.running {
		stops = append(stops, s)
		delete(e.running, k)
	}
	e.mu.Unlock()
	for _, s := range stops {
		s()
	}
}

// responder is a minimal OpenAI-compatible engine that behaves like vLLM
// where the server can tell: the served model name, the API key on /v1/*,
// the open /health, and the error shapes. It is not a language model.
type responder struct {
	d      oakestratest.Deployment
	served string
	key    string
}

func newResponder(d oakestratest.Deployment) *responder {
	r := &responder{d: d, served: cmdFlag(d.Cmd, "--served-model-name"), key: cmdFlag(d.Cmd, "--api-key")}
	if r.served == "" {
		r.served = d.ServiceName
	}
	if r.key == "" {
		for _, kv := range d.Environment {
			if v, ok := strings.CutPrefix(kv, "VLLM_API_KEY="); ok {
				r.key = v
			}
		}
	}
	return r
}

func (r *responder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// vLLM checks the key on /v1 only, so health probes work without it.
	if strings.HasPrefix(req.URL.Path, "/v1/") && !r.authorized(req) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
		return
	}
	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/health":
		// vLLM answers 200 with an empty body.
		w.WriteHeader(http.StatusOK)
	case req.Method == http.MethodGet && req.URL.Path == "/v1/models":
		r.models(w)
	case req.Method == http.MethodPost && req.URL.Path == "/v1/chat/completions":
		r.chat(w, req)
	case req.Method == http.MethodPost && req.URL.Path == "/v1/completions":
		r.completions(w, req)
	case req.Method == http.MethodPost && req.URL.Path == "/v1/embeddings":
		r.embeddings(w, req)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Not Found"})
	}
}

func (r *responder) authorized(req *http.Request) bool {
	if r.key == "" {
		return true
	}
	got, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(r.key)) == 1
}

func cmdFlag(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// openAIError writes the error body recent vLLM versions return.
func openAIError(w http.ResponseWriter, status int, typ, param, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": msg, "type": typ, "param": param, "code": status,
	}})
}

func (r *responder) models(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []map[string]any{{
			"id": r.served, "object": "model", "created": time.Now().Unix(),
			"owned_by": "vllm", "root": "/model", "max_model_len": 8192,
		}},
	})
}

// decode reads a request body and checks its model the way vLLM does: an
// unknown name is a 404, an empty one means the served model.
func (r *responder) decode(w http.ResponseWriter, req *http.Request, dst any, model func() string) bool {
	if err := json.NewDecoder(req.Body).Decode(dst); err != nil {
		openAIError(w, http.StatusBadRequest, "BadRequestError", "", err.Error())
		return false
	}
	if m := model(); m != "" && m != r.served {
		openAIError(w, http.StatusNotFound, "NotFoundError", "model", fmt.Sprintf("The model %#q does not exist.", m))
		return false
	}
	return true
}

type genRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Prompt   any    `json:"prompt"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
}

func (r *responder) reply(prompt string) string {
	if len(prompt) > 80 {
		prompt = prompt[:80] + "..."
	}
	return fmt.Sprintf("fake reply from %s on %s to: %s", r.served, r.d.Node.CandidateName, prompt)
}

func (r *responder) chat(w http.ResponseWriter, req *http.Request) {
	var in genRequest
	if !r.decode(w, req, &in, func() string { return in.Model }) {
		return
	}
	last := ""
	for _, m := range in.Messages {
		if s, ok := m.Content.(string); ok && m.Role == "user" {
			last = s
		}
	}
	text := r.reply(last)
	id := "chatcmpl-fake-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if !in.Stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": r.served,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]string{"role": "assistant", "content": text}}},
			"usage": usage(last, text),
		})
		return
	}
	stream(w, func(emit func(any)) {
		emit(r.chatChunk(id, map[string]string{"role": "assistant", "content": ""}, nil))
		for _, word := range strings.SplitAfter(text, " ") {
			emit(r.chatChunk(id, map[string]string{"content": word}, nil))
		}
		stop := "stop"
		emit(r.chatChunk(id, map[string]string{}, &stop))
	})
}

func (r *responder) chatChunk(id string, delta map[string]string, finish *string) map[string]any {
	return map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": r.served,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
	}
}

func (r *responder) completions(w http.ResponseWriter, req *http.Request) {
	var in genRequest
	if !r.decode(w, req, &in, func() string { return in.Model }) {
		return
	}
	prompt, _ := in.Prompt.(string)
	text := r.reply(prompt)
	id := "cmpl-fake-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if !in.Stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "text_completion", "created": time.Now().Unix(), "model": r.served,
			"choices": []map[string]any{{"index": 0, "text": text, "finish_reason": "stop"}},
			"usage":   usage(prompt, text),
		})
		return
	}
	stream(w, func(emit func(any)) {
		words := strings.SplitAfter(text, " ")
		for i, word := range words {
			var finish any
			if i == len(words)-1 {
				finish = "stop"
			}
			emit(map[string]any{"id": id, "object": "text_completion", "created": time.Now().Unix(), "model": r.served,
				"choices": []map[string]any{{"index": 0, "text": word, "finish_reason": finish}}})
		}
	})
}

func stream(w http.ResponseWriter, body func(emit func(any))) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	body(func(v any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		_ = rc.Flush()
		// A short pause per chunk makes streaming visible end to end.
		time.Sleep(5 * time.Millisecond)
	})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	_ = rc.Flush()
}

func usage(prompt, completion string) map[string]int {
	p, c := len(strings.Fields(prompt))+1, len(strings.Fields(completion))
	return map[string]int{"prompt_tokens": p, "completion_tokens": c, "total_tokens": p + c}
}

// inputs normalizes the "input" of embeddings, which may be a string or a
// list of strings.
func inputs(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// vector is a deterministic unit vector derived from s, so equal inputs
// embed equally and tests can compare results.
func vector(s string) []float64 {
	sum := sha256.Sum256([]byte(s))
	v := make([]float64, 8)
	var norm float64
	for i := range v {
		v[i] = float64(int32(binary.BigEndian.Uint32(sum[i*4:]))) / math.MaxInt32
		norm += v[i] * v[i]
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] /= norm
	}
	return v
}

func (r *responder) embeddings(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Model string `json:"model"`
		Input any    `json:"input"`
	}
	if !r.decode(w, req, &in, func() string { return in.Model }) {
		return
	}
	data := []map[string]any{}
	tokens := 0
	for i, s := range inputs(in.Input) {
		data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": vector(s)})
		tokens += len(strings.Fields(s)) + 1
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list", "model": r.served, "data": data,
		"usage": map[string]int{"prompt_tokens": tokens, "total_tokens": tokens},
	})
}
