package main

import (
	"net/http"

	"github.com/robertjndw/oakestra-openai-api/internal/httpx"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra/oakestratest"
)

// admin is the /__fake API that tests and demos use to simulate nodes
// joining, changing and leaving. It is not part of Oakestra.
type admin struct {
	fake     *oakestratest.Fake
	defaults flags
}

func (a *admin) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /__fake/nodes", a.listNodes)
	mux.HandleFunc("POST /__fake/nodes", a.addNode)
	mux.HandleFunc("PATCH /__fake/nodes/{name}", a.updateNode)
	mux.HandleFunc("DELETE /__fake/nodes/{name}", a.removeNode)
	mux.HandleFunc("GET /__fake/services", a.listServices)
}

// addNodeRequest is the body of POST /__fake/nodes. Every field is
// optional; omitted ones take the command-line defaults.
type addNodeRequest struct {
	Name     string `json:"name"`
	Cluster  string `json:"cluster"`
	IP       string `json:"ip"`
	Arch     string `json:"arch"`
	MemoryMB int    `json:"memory_mb"`
	UsedMB   int    `json:"used_mb"`
	GPUs     *int   `json:"gpus"`
	VRAMMB   *int   `json:"vram_mb"`
	// CSIDrivers replaces the default driver list; [] means none.
	CSIDrivers []string `json:"csi_drivers"`
}

func (a *admin) listNodes(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"nodes": a.fake.Nodes()})
}

func (a *admin) addNode(w http.ResponseWriter, r *http.Request) {
	var req addNodeRequest
	if r.ContentLength != 0 {
		if err := httpx.ReadJSON(r, &req); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}
	spec := nodeSpec(a.defaults, req.Name)
	if req.IP != "" {
		spec.IP = req.IP
	}
	if req.Arch != "" {
		spec.Arch = req.Arch
	}
	if req.MemoryMB > 0 {
		spec.MemoryMB = req.MemoryMB
	}
	if req.UsedMB > 0 {
		spec.UsedMB = req.UsedMB
	}
	if req.GPUs != nil {
		spec.GPUs, spec.NoGPU = *req.GPUs, *req.GPUs == 0
	}
	if req.VRAMMB != nil {
		spec.VRAMMB = *req.VRAMMB
	}
	spec.CSIDrivers = req.CSIDrivers
	cluster := req.Cluster
	if cluster == "" {
		cluster = clusterName(0)
	}
	n, err := a.fake.AddNode(cluster, spec)
	if err != nil {
		httpx.WriteError(w, r, httpx.Conflict("%v", err))
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, n)
}

// updateNodeRequest is the body of PATCH /__fake/nodes/{name}.
type updateNodeRequest struct {
	UsedMB     *int      `json:"used_mb"`
	MemoryMB   *int      `json:"memory_mb"`
	Active     *bool     `json:"active"`
	CSIDrivers *[]string `json:"csi_drivers"`
}

func (a *admin) updateNode(w http.ResponseWriter, r *http.Request) {
	var req updateNodeRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	n, err := a.fake.UpdateNode(r.PathValue("name"), oakestratest.NodeUpdate{
		UsedMB: req.UsedMB, MemoryMB: req.MemoryMB, Active: req.Active, CSIDrivers: req.CSIDrivers,
	})
	if err != nil {
		httpx.WriteError(w, r, httpx.NotFound("%v", err))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, n)
}

func (a *admin) removeNode(w http.ResponseWriter, r *http.Request) {
	if err := a.fake.RemoveNode(r.PathValue("name")); err != nil {
		httpx.WriteError(w, r, httpx.NotFound("%v", err))
		return
	}
	httpx.NoContent(w)
}

func (a *admin) listServices(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"services": a.fake.Services()})
}
