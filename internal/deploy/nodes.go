package deploy

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/httpx"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra"
)

// nodeCacheTTL keeps the nodes page from hitting every cluster's
// abstractor on each refresh, while staying fresh enough to click through.
const nodeCacheTTL = 10 * time.Second

// Node is a worker node as the placement UI shows it.
type Node struct {
	Cluster      string `json:"cluster"`
	Name         string `json:"name"`
	IP           string `json:"ip"`
	Architecture string `json:"architecture"`
	Active       bool   `json:"active"`
	VCPUs        int    `json:"vcpus"`
	// MemoryMB is available, not total, memory.
	MemoryMB   int      `json:"memory_mb"`
	VGPUs      int      `json:"vgpus"`
	VRAMMB     int      `json:"vram_mb"`
	CSIDrivers []string `json:"csi_drivers"`
	// ModelpackCSI is whether the node advertises the configured model CSI
	// driver, without which a deployment there can never be scheduled.
	ModelpackCSI bool `json:"modelpack_csi"`
}

// NodeList is every node of every cluster whose abstractor answered.
type NodeList struct {
	Nodes []Node `json:"nodes"`
	// Warnings name clusters whose nodes could not be listed. One broken
	// abstractor must not hide the nodes of the others.
	Warnings []string `json:"warnings"`

	failedClusters []string
}

// Nodes returns the node list, cached for about ten seconds.
func (m *Manager) Nodes(ctx context.Context) (NodeList, error) {
	return m.nodeList(ctx, false)
}

// RefreshNodes reads the node list from Oakestra, bypassing the cache.
func (m *Manager) RefreshNodes(ctx context.Context) (NodeList, error) {
	return m.nodeList(ctx, true)
}

func (m *Manager) nodeList(ctx context.Context, fresh bool) (NodeList, error) {
	m.nodesMu.Lock()
	defer m.nodesMu.Unlock()
	if fresh || m.nodesAt.IsZero() || m.now().Sub(m.nodesAt) > nodeCacheTTL {
		nl, err := m.fetchNodes(ctx)
		if err != nil {
			return NodeList{}, err
		}
		m.nodes, m.nodesAt = nl, m.now()
	}
	nl := m.nodes
	nl.Nodes = slices.Clone(nl.Nodes)
	nl.Warnings = slices.Clone(nl.Warnings)
	return nl, nil
}

func (m *Manager) fetchNodes(ctx context.Context) (NodeList, error) {
	clusters, err := m.oak.ListClusters(ctx)
	if err != nil {
		return NodeList{}, &httpx.Error{Status: http.StatusServiceUnavailable, Code: httpx.CodeUnavailable,
			Message: fmt.Sprintf("cannot list Oakestra clusters: %v", err), Err: err}
	}
	type result struct {
		nodes []oakestra.NodeResource
		err   error
	}
	results := make([]result, len(clusters))
	var wg sync.WaitGroup
	for i, cl := range clusters {
		wg.Go(func() {
			nodes, err := m.oak.ListNodes(ctx, cl)
			results[i] = result{nodes, err}
		})
	}
	wg.Wait()

	nl := NodeList{Nodes: []Node{}, Warnings: []string{}}
	for i, cl := range clusters {
		name := cl.DisplayName()
		if err := results[i].err; err != nil {
			nl.Warnings = append(nl.Warnings, fmt.Sprintf("cluster %s: cannot list nodes: %v", name, err))
			nl.failedClusters = append(nl.failedClusters, name)
			continue
		}
		for _, n := range results[i].nodes {
			nl.Nodes = append(nl.Nodes, m.nodeView(name, n))
		}
	}
	return nl, nil
}

func (m *Manager) nodeView(cluster string, n oakestra.NodeResource) Node {
	name := n.CandidateName
	if name == "" {
		name = n.Host
	}
	drivers := make([]string, 0, len(n.CSIDrivers))
	for _, d := range n.CSIDrivers {
		drivers = append(drivers, d.Name)
	}
	return Node{
		Cluster:      cluster,
		Name:         name,
		IP:           n.IP,
		Architecture: n.Architecture,
		Active:       n.Active,
		VCPUs:        n.VCPUs,
		MemoryMB:     n.MemoryMB,
		VGPUs:        n.VGPUs,
		VRAMMB:       int(math.Round(n.VRAMMB)),
		CSIDrivers:   drivers,
		ModelpackCSI: slices.Contains(drivers, m.cfg.ModelCSIDriver),
	}
}

func (nl NodeList) find(cluster, node string) (Node, bool) {
	for _, n := range nl.Nodes {
		if n.Cluster == cluster && n.Name == node {
			return n, true
		}
	}
	return Node{}, false
}
