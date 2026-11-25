package oakestratest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/oakestra"
	"github.com/robertjndw/oakestra-openai-api/internal/oakestra/oakestratest"
)

func driverNames(n oakestra.NodeResource) []string {
	var out []string
	for _, d := range n.CSIDrivers {
		out = append(out, d.Name)
	}
	return out
}

// The abstractor reports each node's own driver list in the node engine's
// object format, and the cluster list aggregates them as names.
func TestCSIDriversReported(t *testing.T) {
	fake := oakestratest.New(oakestratest.Options{})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	fake.AddCluster("edge", "10.0.0.2")
	for _, spec := range []oakestratest.NodeSpec{
		{Name: "both"},
		{Name: "hostpath", CSIDrivers: []string{oakestra.HostpathCSIDriver}},
		{Name: "none", CSIDrivers: []string{}},
	} {
		if _, err := fake.AddNode("edge", spec); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := http.Get(srv.URL + oakestratest.AbstractorPath("edge") + "/api/v1/resources/")
	if err != nil {
		t.Fatal(err)
	}
	var raw []struct {
		Name string `json:"candidate_name"`
		CSI  []struct {
			Name     string `json:"csi_driver_name"`
			Endpoint string `json:"csi_driver_endpoint"`
		} `json:"csi_drivers"`
	}
	err = json.NewDecoder(resp.Body).Decode(&raw)
	_ = resp.Body.Close()
	if err != nil || len(raw) != 3 {
		t.Fatalf("resources = %+v, %v", raw, err)
	}
	if len(raw[0].CSI) != 2 || raw[0].CSI[1].Name != oakestra.ModelpackCSIDriver || raw[0].CSI[1].Endpoint == "" {
		t.Errorf("default node drivers = %+v", raw[0].CSI)
	}
	if len(raw[1].CSI) != 1 || raw[1].CSI[0].Name != oakestra.HostpathCSIDriver {
		t.Errorf("hostpath-only node drivers = %+v", raw[1].CSI)
	}
	if len(raw[2].CSI) != 0 {
		t.Errorf("driverless node drivers = %+v", raw[2].CSI)
	}

	c, err := oakestra.New(oakestratest.ClientOptions(srv.URL, "ooa", "pw"))
	if err != nil {
		t.Fatal(err)
	}
	clusters, err := c.ListClusters(t.Context())
	if err != nil || len(clusters) != 1 {
		t.Fatalf("clusters = %+v, %v", clusters, err)
	}
	var agg []string
	for _, d := range clusters[0].CSIDrivers {
		agg = append(agg, d.Name)
	}
	if !slices.Equal(agg, []string{oakestra.HostpathCSIDriver, oakestra.ModelpackCSIDriver}) {
		t.Errorf("cluster drivers = %v", agg)
	}

	none := []string{}
	n, err := fake.UpdateNode("both", oakestratest.NodeUpdate{CSIDrivers: &none})
	if err != nil || len(n.CSIDrivers) != 0 {
		t.Fatalf("after removing drivers: %v, %v", driverNames(n), err)
	}
}

func deployModel(t *testing.T, node string) (oakestratest.ServiceView, *oakestratest.Fake, []oakestratest.Deployment) {
	t.Helper()
	var mu sync.Mutex
	var deployed []oakestratest.Deployment
	fake := oakestratest.New(oakestratest.Options{
		OnDeploy: func(_ context.Context, d oakestratest.Deployment) error {
			mu.Lock()
			defer mu.Unlock()
			deployed = append(deployed, d)
			return nil
		},
	})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	fake.AddCluster("edge", "10.0.0.2")
	if _, err := fake.AddNode("edge", oakestratest.NodeSpec{Name: "gpu1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.AddNode("edge", oakestratest.NodeSpec{Name: "cpu1", CSIDrivers: []string{oakestra.HostpathCSIDriver}}); err != nil {
		t.Fatal(err)
	}
	c, err := oakestra.New(oakestratest.ClientOptions(srv.URL, "ooa", "pw"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	app, _ := oakestra.NewApplication("openaiapi", "ooa", "")
	info, err := oakestra.EnsureApplication(ctx, c, app)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := oakestra.BuildMicroservice(oakestra.ServiceSpec{
		Name: oakestra.HashedName("qwen", node, 6), Namespace: "ooa", Image: "vllm/vllm-openai:v0.29.0",
		Command:  []string{"vllm", "serve", "/model"},
		MemoryMB: 1024, HostPort: 18000, ContainerPort: 8000, Cluster: "edge", Node: node,
		Volumes: []oakestra.Volume{{
			VolumeID: "d_1", CSIDriver: oakestra.ModelpackCSIDriver, MountPath: "/model",
			Config: map[string]string{oakestra.ModelpackReferenceKey: "registry.example/qwen:1"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sid, err := c.CreateService(ctx, info.ApplicationID, ms)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeployInstance(ctx, sid); err != nil {
		t.Fatal(err)
	}
	svc, ok := fake.Service(sid)
	if !ok {
		t.Fatal("service vanished")
	}
	mu.Lock()
	defer mu.Unlock()
	return svc, fake, slices.Clone(deployed)
}

func TestSchedulerRejectsMissingCSIDriver(t *testing.T) {
	svc, _, deployed := deployModel(t, "cpu1")
	if len(svc.InstanceList) != 0 || len(deployed) != 0 {
		t.Fatalf("instances %+v, deployed %+v", svc.InstanceList, deployed)
	}
	if svc.Status != oakestra.StatusNoActiveClusterWithCap {
		t.Errorf("status = %q", svc.Status)
	}
	want := "node cpu1 does not have required CSI driver model.csi.modelpack.org (available: [csi.oakestra.io/hostpath])"
	if svc.StatusDetail != want {
		t.Errorf("status_detail = %q, want %q", svc.StatusDetail, want)
	}
}

func TestSchedulerPlacesWhenDriverPresent(t *testing.T) {
	svc, _, deployed := deployModel(t, "gpu1")
	if len(svc.InstanceList) != 1 || svc.InstanceList[0].Status != oakestra.StatusRunning {
		t.Fatalf("instances %+v", svc.InstanceList)
	}
	if len(deployed) != 1 || deployed[0].Node.CandidateName != "gpu1" || len(deployed[0].Volumes) != 1 {
		t.Fatalf("deployed %+v", deployed)
	}
	if !strings.HasPrefix(svc.SLA.Volumes[0].Config[oakestra.ModelpackReferenceKey], "registry.example/") {
		t.Fatalf("SLA volumes = %+v", svc.SLA.Volumes)
	}
}

func TestNodeIPMayBeAHostname(t *testing.T) {
	svc, fake, _ := deployModel(t, "gpu1")
	if svc.InstanceList[0].PublicIP != "127.0.0.1" {
		t.Fatalf("default public ip = %q", svc.InstanceList[0].PublicIP)
	}
	n, err := fake.AddNode("edge", oakestratest.NodeSpec{Name: "dockerized", IP: "ooa-fakeoak"})
	if err != nil || n.IP != "ooa-fakeoak" {
		t.Fatalf("node = %+v, %v", n, err)
	}
}

func TestRemoveNodeFailsItsInstances(t *testing.T) {
	var mu sync.Mutex
	var undeployed []oakestratest.Deployment
	fake := oakestratest.New(oakestratest.Options{
		OnUndeploy: func(d oakestratest.Deployment) {
			mu.Lock()
			defer mu.Unlock()
			undeployed = append(undeployed, d)
		},
	})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	fake.AddCluster("edge", "10.0.0.2")
	if _, err := fake.AddNode("edge", oakestratest.NodeSpec{Name: "dgx1"}); err != nil {
		t.Fatal(err)
	}
	c, err := oakestra.New(oakestratest.ClientOptions(srv.URL, "ooa", "pw"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	app, _ := oakestra.NewApplication("openaiapi", "ooa", "")
	info, err := c.CreateApplication(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := oakestra.BuildMicroservice(oakestra.ServiceSpec{
		Name: "e1", Namespace: "ooa", Image: "img", MemoryMB: 1, HostPort: 18001, ContainerPort: 8000,
		Cluster: "edge", Node: "dgx1",
	})
	if err != nil {
		t.Fatal(err)
	}
	sid, err := c.CreateService(ctx, info.ApplicationID, ms)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeployInstance(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if err := fake.RemoveNode("dgx1"); err != nil {
		t.Fatal(err)
	}
	svc, _ := fake.Service(sid)
	if len(svc.InstanceList) != 1 || svc.InstanceList[0].Status != oakestra.StatusFailed {
		t.Fatalf("instances = %+v", svc.InstanceList)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(undeployed) != 1 || undeployed[0].HostPort != 18001 {
		t.Fatalf("undeployed = %+v", undeployed)
	}
}

func TestInactiveNodeLooksStale(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	fake := oakestratest.New(oakestratest.Options{Now: func() time.Time { return now }})
	fake.AddCluster("edge", "10.0.0.2")
	if _, err := fake.AddNode("edge", oakestratest.NodeSpec{Name: "dgx1"}); err != nil {
		t.Fatal(err)
	}
	off := false
	n, err := fake.UpdateNode("dgx1", oakestratest.NodeUpdate{Active: &off})
	if err != nil {
		t.Fatal(err)
	}
	if n.Active || !n.LastModified.Before(now.Add(-30*time.Second)) {
		t.Fatalf("inactive node = active %v, last modified %v", n.Active, n.LastModified)
	}
}
