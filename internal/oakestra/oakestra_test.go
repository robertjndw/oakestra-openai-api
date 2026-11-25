package oakestra

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// A trimmed real response from a cluster resource abstractor.
const nodeJSON = `{
  "_id": "6650f0c2a1b2c3d4e5f60718",
  "candidate_name": "dgx1",
  "host": "dgx1",
  "ip": "10.0.0.11",
  "port": "50103",
  "architecture": "arm64",
  "vcpus": 20,
  "cpu_percent": 3.5,
  "memory": 117000,
  "memory_percent": 8.4,
  "vram": 0,
  "vram_percent": 0,
  "vgpus": 1,
  "gpu_driver": "580.95",
  "virtualization": ["docker"],
  "csi_drivers": [
    {"csi_driver_name": "csi.oakestra.io/hostpath", "csi_driver_endpoint": "/run/csi/hostpath.sock"},
    {"csi_driver_name": "model.csi.modelpack.org", "csi_driver_endpoint": "/run/csi/modelpack.sock"}
  ],
  "active": true,
  "last_modified_timestamp": 1767225600.25
}`

func TestNodeResourceDecodes(t *testing.T) {
	var n NodeResource
	if err := json.Unmarshal([]byte(nodeJSON), &n); err != nil {
		t.Fatal(err)
	}
	if n.CandidateName != "dgx1" || n.Architecture != "arm64" || n.MemoryMB != 117000 || n.VGPUs != 1 {
		t.Errorf("node = %+v", n)
	}
	if len(n.CSIDrivers) != 2 || n.CSIDrivers[0].Name != HostpathCSIDriver || n.CSIDrivers[1].Name != ModelpackCSIDriver {
		t.Errorf("csi = %+v", n.CSIDrivers)
	}
	want := time.Unix(1767225600, 250_000_000).UTC()
	if !n.LastModified.Equal(want) {
		t.Errorf("LastModified = %v, want %v", n.LastModified, want)
	}
	if n.Port != "50103" {
		t.Errorf("Port = %q", n.Port)
	}
}

func TestClusterDecodesLooseTypes(t *testing.T) {
	const in = `{"_id":"c1","cluster_name":"edge","ip":"10.0.0.2","port":10100,
		"csi_drivers":["csi.oakestra.io/hostpath"],"active":true,"memory":1234.5,
		"last_modified_timestamp":null}`
	var c Cluster
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatal(err)
	}
	if c.Port != "10100" || c.CSIDrivers[0].Name != HostpathCSIDriver || !c.LastModified.IsZero() {
		t.Errorf("cluster = %+v", c)
	}
}

func TestInstanceHostPortFlexible(t *testing.T) {
	for _, in := range []string{`{"host_port":"50011"}`, `{"host_port":50011}`} {
		var i Instance
		if err := json.Unmarshal([]byte(in), &i); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if i.HostPort != 50011 {
			t.Errorf("%s: HostPort = %d", in, i.HostPort)
		}
	}
}

func TestMicroserviceOmitsEmpty(t *testing.T) {
	b, err := json.Marshal(Microservice{
		MicroserviceName: "qwen1a2b3c", MicroserviceNamespace: "ooa",
		Virtualization: VirtualizationContainer, Code: "vllm/vllm-openai:v0.29.0",
		Memory: 1024, VGPUs: 1, Port: "18000:8000",
		Constraints: []Constraint{{Type: ConstraintDirect, Cluster: "edge", Node: "dgx1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"microservice_name":"qwen1a2b3c","microservice_namespace":"ooa","virtualization":"container","code":"vllm/vllm-openai:v0.29.0","memory":1024,"vgpus":1,"port":"18000:8000","constraints":[{"type":"direct","cluster":"edge","node":"dgx1"}]}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
}

func TestAPIErrorIs(t *testing.T) {
	err := fmt.Errorf("deploy: %w", &APIError{Op: "get service", Status: 404, Message: "not found"})
	if !errors.Is(err, ErrNotFound) {
		t.Error("404 does not match ErrNotFound")
	}
	if errors.Is(err, ErrConflict) {
		t.Error("404 matches ErrConflict")
	}
	if !errors.Is(&APIError{Status: 422}, ErrInvalid) {
		t.Error("422 does not match ErrInvalid")
	}
}
