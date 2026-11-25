package deploy

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/oakestra"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra/oakestratest"
)

func TestNodes(t *testing.T) {
	h := newHarness(t, nil)
	h.addNode("c1", oakestratest.NodeSpec{Name: "cpu", NoGPU: true, VRAMMB: 0, CSIDrivers: []string{}, Arch: "amd64"})
	h.fake.AddCluster("broken", "10.0.0.9")
	h.addNode("broken", oakestratest.NodeSpec{Name: "hidden"})
	h.fake.Fail(oakestratest.Failure{Path: oakestratest.AbstractorPath("broken"), Status: http.StatusBadGateway, Message: "abstractor down"})

	nl, err := h.m.Nodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(nl.Nodes) != 2 {
		t.Fatalf("nodes = %+v", nl.Nodes)
	}
	n1, cpu := nl.Nodes[0], nl.Nodes[1]
	if n1.Cluster != "c1" || n1.Name != "n1" || n1.IP != "127.0.0.1" || !n1.Active || !n1.ModelpackCSI ||
		n1.VGPUs != 1 || n1.MemoryMB != 131072-4096 || n1.VCPUs != 20 || n1.Architecture != "arm64" ||
		!slices.Contains(n1.CSIDrivers, oakestra.ModelpackCSIDriver) {
		t.Errorf("n1 = %+v", n1)
	}
	if cpu.ModelpackCSI || cpu.CSIDrivers == nil || len(cpu.CSIDrivers) != 0 || cpu.VGPUs != 0 || cpu.Architecture != "amd64" {
		t.Errorf("cpu = %+v", cpu)
	}
	if len(nl.Warnings) != 1 || !strings.Contains(nl.Warnings[0], "broken") {
		t.Errorf("warnings = %q", nl.Warnings)
	}

	// Cached for a while, fresh on demand or once the cache is old.
	h.addNode("c1", oakestratest.NodeSpec{Name: "late"})
	if nl, _ := h.m.Nodes(t.Context()); len(nl.Nodes) != 2 {
		t.Errorf("cache not used: %d nodes", len(nl.Nodes))
	}
	if nl, _ := h.m.RefreshNodes(t.Context()); len(nl.Nodes) != 3 {
		t.Errorf("refresh: %d nodes, want 3", len(nl.Nodes))
	}
	h.addNode("c1", oakestratest.NodeSpec{Name: "later"})
	h.advance(11 * time.Second)
	if nl, _ := h.m.Nodes(t.Context()); len(nl.Nodes) != 4 {
		t.Errorf("expired cache: %d nodes, want 4", len(nl.Nodes))
	}

	// A node in a cluster that cannot be listed is not "not found".
	m := h.model("qwen")
	_, err = h.m.CreateDeployment(t.Context(), m.ID, "broken", "hidden")
	assertStatus(t, err, http.StatusServiceUnavailable)

	h.down.Store(true)
	_, err = h.m.RefreshNodes(t.Context())
	assertStatus(t, err, http.StatusServiceUnavailable)
	if err := h.m.CheckOakestra(t.Context()); err == nil {
		t.Error("CheckOakestra passed while Oakestra is down")
	}
}
