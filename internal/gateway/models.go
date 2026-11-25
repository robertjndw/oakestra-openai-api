package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/robertjndw/oakestra-openai-api/internal/httpx"
	"github.com/robertjndw/oakestra-openai-api/internal/scheduler"
)

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (g *Gateway) modelIDs() []string {
	st := g.state.Load()
	ids := make([]string, 0, len(st.byModel)+1)
	for name := range st.byModel {
		ids = append(ids, name)
	}
	slices.Sort(ids)
	if len(st.auto) > 0 {
		ids = append(ids, AutoModel)
	}
	return ids
}

func (g *Gateway) model(id string) modelObject {
	return modelObject{ID: id, Object: "model", Created: g.started.Unix(), OwnedBy: "oakestra"}
}

func (g *Gateway) listModels(w http.ResponseWriter, _ *http.Request) {
	data := []modelObject{}
	for _, id := range g.modelIDs() {
		data = append(data, g.model(id))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (g *Gateway) getModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !slices.Contains(g.modelIDs(), id) {
		writeError(w, http.StatusNotFound, typeInvalidRequest, codeModelNotFound, "model",
			fmt.Sprintf("the model %q does not exist", id))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, g.model(id))
}

// Complete runs a non-streaming chat completion against a ready deployment
// of the named model and returns the assistant text. It is meant for
// scheduler.Deps.Complete, so a strategy can consult a decision model.
func (g *Gateway) Complete(ctx context.Context, model string, msgs []scheduler.Message) (string, error) {
	eps := g.state.Load().byModel[model]
	if len(eps) == 0 {
		return "", fmt.Errorf("gateway: no ready deployment of model %q", model)
	}
	type chatMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	cm := make([]chatMessage, len(msgs))
	for i, m := range msgs {
		cm[i] = chatMessage{m.Role, m.Content}
	}
	messages, err := json.Marshal(cm)
	if err != nil {
		return "", err
	}
	fields := map[string]json.RawMessage{
		"messages": messages,
		"stream":   json.RawMessage("false"),
	}

	ranked := g.rank(ctx, scheduler.Request{API: "chat.completions"}, eps, false)
	var lastErr error
	for _, e := range ranked[:min(g.opts.MaxAttempts, len(ranked))] {
		resp, done, err := g.send(ctx, e, http.MethodPost, "/v1/chat/completions", "", http.Header{}, fields)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			g.failed(e)
			lastErr = err
			continue
		}
		text, err := readCompletion(resp)
		done()
		return text, err
	}
	return "", fmt.Errorf("gateway: no engine for model %q could be reached: %w", model, lastErr)
}

func readCompletion(resp *http.Response) (string, error) {
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("gateway: read completion: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gateway: engine answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("gateway: decode completion: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("gateway: completion has no choices")
	}
	return out.Choices[0].Message.Content, nil
}
