package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/robertjndw/oakestra-openai-api/internal/oakestra"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra/oakestratest"
)

func noEnv(string) (string, bool) { return "", false }

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %T", ln.Addr())
	}
	_ = ln.Close()
	return addr.Port
}

type stack struct {
	srv    *httptest.Server
	client *oakestra.HTTPClient
}

func newStack(t *testing.T, args ...string) *stack {
	t.Helper()
	f, err := parseFlags(args, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	h, closeEngines, err := newHandler(f, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeEngines)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := oakestra.New(oakestratest.ClientOptions(srv.URL, "ooa", "pw"))
	if err != nil {
		t.Fatal(err)
	}
	return &stack{srv: srv, client: c}
}

// deploy creates and deploys a vLLM-shaped model service on node and
// returns the service after scheduling.
func (s *stack) deploy(t *testing.T, node string, port int) oakestra.Service {
	t.Helper()
	ctx := t.Context()
	app, _ := oakestra.NewApplication("openaiapi", "ooa", "")
	info, err := oakestra.EnsureApplication(ctx, s.client, app)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := oakestra.BuildMicroservice(oakestra.ServiceSpec{
		Name: oakestra.HashedName("qwen", node+strconv.Itoa(port), 8), Namespace: "ooa", Image: "vllm/vllm-openai:v0.29.0",
		Command:  []string{"vllm", "serve", "/model", "--served-model-name", "qwen3", "--port", "8000", "--api-key", "ek-1"},
		HostPort: port, ContainerPort: 8000, MemoryMB: 1024, Cluster: "cluster1", Node: node,
		Volumes: []oakestra.Volume{{
			VolumeID: "d_" + strconv.Itoa(port), CSIDriver: oakestra.ModelpackCSIDriver, MountPath: "/model",
			Config: map[string]string{oakestra.ModelpackReferenceKey: "registry.example/qwen3:1"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sid, err := s.client.CreateService(ctx, info.ApplicationID, ms)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.client.DeployInstance(ctx, sid); err != nil {
		t.Fatal(err)
	}
	svc, err := s.client.GetService(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func do(t *testing.T, method, url, key, body string) (status int, respBody string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSeedSpreadsNodesOverClusters(t *testing.T) {
	s := newStack(t, "-nodes", "5", "-clusters", "2", "-gpus", "0", "-no-modelpack", "node3")
	clusters, err := s.client.ListClusters(t.Context())
	if err != nil || len(clusters) != 2 {
		t.Fatalf("clusters = %+v, %v", clusters, err)
	}
	nodes, err := s.client.ListNodes(t.Context(), clusters[0])
	if err != nil || len(nodes) != 3 {
		t.Fatalf("cluster1 nodes = %d, %v", len(nodes), err)
	}
	if n := nodes[0]; n.Architecture != "amd64" || n.VGPUs != 0 || n.IP != "127.0.0.1" || len(n.CSIDrivers) != 2 {
		t.Fatalf("node = %+v", n)
	}
	// node3 is the second node of cluster1 (round-robin: 1, 3, 5).
	if n := nodes[1]; n.CandidateName != "node3" || len(n.CSIDrivers) != 1 || n.CSIDrivers[0].Name != oakestra.HostpathCSIDriver {
		t.Fatalf("node3 = %+v", n)
	}
}

func TestSpawnedEngineBehavesLikeVLLM(t *testing.T) {
	s := newStack(t)
	port := freePort(t)
	svc := s.deploy(t, "node1", port)
	if len(svc.InstanceList) != 1 || svc.InstanceList[0].Status != oakestra.StatusRunning {
		t.Fatalf("service = %+v", svc)
	}
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	if code, body := do(t, http.MethodGet, base+"/health", "", ""); code != http.StatusOK || body != "" {
		t.Fatalf("health without key: %d %q", code, body)
	}
	if code, body := do(t, http.MethodGet, base+"/v1/models", "", ""); code != http.StatusUnauthorized || !strings.Contains(body, "Unauthorized") {
		t.Fatalf("models without key: %d %s", code, body)
	}
	if code, _ := do(t, http.MethodGet, base+"/v1/models", "wrong", ""); code != http.StatusUnauthorized {
		t.Fatalf("models with wrong key: %d", code)
	}
	code, body := do(t, http.MethodGet, base+"/v1/models", "ek-1", "")
	if code != http.StatusOK || !strings.Contains(body, `"id":"qwen3"`) {
		t.Fatalf("models: %d %s", code, body)
	}
	if code, body := do(t, http.MethodPost, base+"/v1/chat/completions", "ek-1", `{"model":"other","messages":[]}`); code != http.StatusNotFound || !strings.Contains(body, "does not exist") {
		t.Fatalf("unknown model: %d %s", code, body)
	}
	code, body = do(t, http.MethodPost, base+"/v1/completions", "ek-1", `{"model":"qwen3","prompt":"hi"}`)
	if code != http.StatusOK || !strings.Contains(body, `"model":"qwen3"`) {
		t.Fatalf("completion: %d %s", code, body)
	}

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ek-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("stream content type %q", ct)
	}
	var chunks int
	var done bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if line == "data: [DONE]" {
			done = true
		} else if data, ok := strings.CutPrefix(line, "data: "); ok {
			var chunk struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal([]byte(data), &chunk); err != nil || chunk.Model != "qwen3" {
				t.Errorf("chunk %s: %v", data, err)
			}
			chunks++
		}
	}
	_ = resp.Body.Close()
	if !done || chunks < 3 {
		t.Fatalf("stream: %d chunks, done %v", chunks, done)
	}

	code, body = do(t, http.MethodPost, base+"/v1/embeddings", "ek-1", `{"model":"qwen3","input":["a","b"]}`)
	var emb struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &emb)
	if code != http.StatusOK || len(emb.Data) != 2 || len(emb.Data[0].Embedding) != 8 {
		t.Fatalf("embeddings: %d %s", code, body)
	}

	if err := s.client.DeleteService(t.Context(), svc.ID); err != nil {
		t.Fatal(err)
	}
	if resp, err := http.Get(base + "/health"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("engine still serving after the service was deleted")
	}
}

func TestPortTakenFailsInstance(t *testing.T) {
	s := newStack(t)
	port := freePort(t)
	if svc := s.deploy(t, "node1", port); svc.InstanceList[0].Status != oakestra.StatusRunning {
		t.Fatalf("first = %+v", svc.InstanceList)
	}
	svc := s.deploy(t, "node2", port)
	if len(svc.InstanceList) != 1 || svc.InstanceList[0].Status != oakestra.StatusFailed || svc.InstanceList[0].StatusDetail == "" {
		t.Fatalf("second = %+v", svc.InstanceList)
	}
}

func TestNodeWithoutModelpackRefusesModels(t *testing.T) {
	s := newStack(t, "-no-modelpack", "node2")
	svc := s.deploy(t, "node2", freePort(t))
	if len(svc.InstanceList) != 0 || svc.Status != oakestra.StatusNoActiveClusterWithCap ||
		!strings.Contains(svc.StatusDetail, oakestra.ModelpackCSIDriver) {
		t.Fatalf("service = %+v", svc)
	}
}

func TestAdminNodes(t *testing.T) {
	s := newStack(t, "-nodes", "2")
	if code, body := do(t, http.MethodPost, s.srv.URL+"/__fake/nodes", "", ""); code != http.StatusCreated || !strings.Contains(body, `"node3"`) {
		t.Fatalf("add default node: %d %s", code, body)
	}
	code, body := do(t, http.MethodPost, s.srv.URL+"/__fake/nodes", "", `{"name":"cpu","gpus":0,"csi_drivers":["csi.oakestra.io/hostpath"]}`)
	if code != http.StatusCreated || strings.Contains(body, oakestra.ModelpackCSIDriver) {
		t.Fatalf("add cpu node: %d %s", code, body)
	}
	if code, _ := do(t, http.MethodPost, s.srv.URL+"/__fake/nodes", "", `{"name":"cpu"}`); code != http.StatusConflict {
		t.Fatalf("duplicate add = %d", code)
	}
	code, body = do(t, http.MethodPatch, s.srv.URL+"/__fake/nodes/cpu", "", `{"active":false,"csi_drivers":[]}`)
	if code != http.StatusOK || !strings.Contains(body, `"active":false`) || !strings.Contains(body, `"csi_drivers":[]`) {
		t.Fatalf("patch: %d %s", code, body)
	}
	if code, _ := do(t, http.MethodDelete, s.srv.URL+"/__fake/nodes/node1", "", ""); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	if code, _ := do(t, http.MethodDelete, s.srv.URL+"/__fake/nodes/node1", "", ""); code != http.StatusNotFound {
		t.Fatalf("second delete = %d", code)
	}
	code, body = do(t, http.MethodGet, s.srv.URL+"/__fake/nodes", "", "")
	if code != http.StatusOK || strings.Contains(body, `"node1"`) || !strings.Contains(body, `"cpu"`) {
		t.Fatalf("list: %d %s", code, body)
	}
}

func TestParseFlags(t *testing.T) {
	for _, args := range [][]string{
		{"-clusters", "0"},
		{"-node-used-mb", "200000"},
		{"-user", "ooa"},
		{"-no-modelpack", "node9"},
		{"-node-ip", ""},
	} {
		if _, err := parseFlags(args, noEnv); err == nil {
			t.Errorf("%v accepted", args)
		}
	}

	env := map[string]string{
		"OOA_FAKEOAK_NODE_IP":      "ooa-fakeoak",
		"OOA_FAKEOAK_ENGINE_BIND":  "0.0.0.0",
		"OOA_FAKEOAK_NODES":        "4",
		"OOA_FAKEOAK_NO_MODELPACK": "node3, node4",
		"OOA_FAKEOAK_PUBLIC_URL":   "http://ooa-fakeoak:10000/",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	f, err := parseFlags([]string{"-nodes", "5"}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	if f.nodeIP != "ooa-fakeoak" || f.engineBind != "0.0.0.0" || f.nodes != 5 || strings.Join(f.noModelpack, ",") != "node3,node4" {
		t.Fatalf("flags = %+v", f)
	}
	if got := abstractorOverrides(f); got != "cluster1=http://ooa-fakeoak:10000/cluster/cluster1" {
		t.Fatalf("overrides = %q", got)
	}

	env["OOA_FAKEOAK_NODES"] = "many"
	if _, err := parseFlags(nil, lookup); err == nil || !strings.Contains(err.Error(), "OOA_FAKEOAK_NODES") {
		t.Fatalf("bad env value: %v", err)
	}
}

func TestCmdFlag(t *testing.T) {
	args := []string{"vllm", "serve", "/model", "--served-model-name=qwen3", "--api-key", "k1", "--port"}
	if cmdFlag(args, "--served-model-name") != "qwen3" || cmdFlag(args, "--api-key") != "k1" || cmdFlag(args, "--port") != "" {
		t.Fatalf("cmdFlag misparsed %q", args)
	}
	r := newResponder(oakestratest.Deployment{ServiceName: "svc", Environment: []string{"VLLM_API_KEY=env-key"}})
	if r.served != "svc" || r.key != "env-key" {
		t.Fatalf("fallbacks: served %q key %q", r.served, r.key)
	}
}
