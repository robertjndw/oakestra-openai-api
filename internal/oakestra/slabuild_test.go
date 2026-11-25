package oakestra

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata")

func golden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from golden file\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

const testDeploymentID = "d_01kbx7m2c3q4r5s6t7v8w9x0yz"

// modelSpec is a vLLM engine serving a ModelPack model mounted by the
// upstream CSI driver, the service every deployment renders.
func modelSpec() ServiceSpec {
	return ServiceSpec{
		Name:      HashedName("qwen3", testDeploymentID, 8),
		Namespace: "ooa",
		Image:     "vllm/vllm-openai:v0.29.0",
		Command: []string{
			"vllm", "serve", "/model",
			"--served-model-name", "qwen3-8b",
			"--port", "8000",
			"--api-key", "ek-3f9a1c",
			"--max-model-len", "8192",
		},
		HostPort:      18000,
		ContainerPort: 8000,
		MemoryMB:      24576,
		VGPUs:         1,
		Cluster:       "cluster1",
		Node:          "gpu-node-1",
		Volumes: []Volume{{
			VolumeID:  testDeploymentID,
			CSIDriver: ModelpackCSIDriver,
			MountPath: "/model",
			Config:    map[string]string{ModelpackReferenceKey: "ghcr.io/example/qwen3-8b:v1"},
		}},
	}
}

func TestModelServiceSLAGolden(t *testing.T) {
	ms, err := BuildMicroservice(modelSpec())
	if err != nil {
		t.Fatal(err)
	}
	app := ApplicationInfo{ApplicationID: "665000000000000000000001", ApplicationName: "openaiapi", ApplicationNamespace: "ooa"}
	golden(t, "model_service_sla.json", ServiceSLA("ooa", app, ms))
}

func TestApplicationSLAGolden(t *testing.T) {
	app, err := NewApplication("openaiapi", "ooa", "OpenAI-compatible model serving")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "application_sla.json", ApplicationSLA("ooa", app))
}

func TestBuildMicroserviceKeepsVolumesIndependent(t *testing.T) {
	spec := modelSpec()
	spec.Volumes = append(spec.Volumes, Volume{VolumeID: "/var/cache/ooa", CSIDriver: HostpathCSIDriver, MountPath: "/root/.cache"})
	ms, err := BuildMicroservice(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms.Volumes) != 2 || ms.Volumes[1].CSIDriver != HostpathCSIDriver || ms.Volumes[1].Config != nil {
		t.Fatalf("volumes = %+v", ms.Volumes)
	}
	spec.Volumes[0].Config[ModelpackReferenceKey] = "changed"
	if ms.Volumes[0].Config[ModelpackReferenceKey] != "ghcr.io/example/qwen3-8b:v1" {
		t.Fatal("the rendered volume shares its config map with the spec")
	}
}

func TestBuildMicroserviceValidates(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*ServiceSpec)
		want string
	}{
		{"long name", func(s *ServiceSpec) { s.Name = strings.Repeat("a", 33) }, "name"},
		{"dash in name", func(s *ServiceSpec) { s.Name = "q-wen" }, "name"},
		{"bad namespace", func(s *ServiceSpec) { s.Namespace = "" }, "namespace"},
		{"no image", func(s *ServiceSpec) { s.Image = "" }, "image"},
		{"empty executable", func(s *ServiceSpec) { s.Command = []string{"", "serve"} }, "executable"},
		{"no cluster", func(s *ServiceSpec) { s.Cluster = "" }, "direct constraint"},
		{"no node", func(s *ServiceSpec) { s.Node = "" }, "direct constraint"},
		{"no memory", func(s *ServiceSpec) { s.MemoryMB = 0 }, "memory"},
		{"negative gpus", func(s *ServiceSpec) { s.VGPUs = -1 }, "vgpus"},
		{"bad port", func(s *ServiceSpec) { s.ContainerPort = 0 }, "ports"},
		{"bad env", func(s *ServiceSpec) { s.Env = map[string]string{"A=B": "c"} }, "environment"},
		{"no driver", func(s *ServiceSpec) { s.Volumes[0].CSIDriver = "" }, "csi_driver"},
		{"no volume id", func(s *ServiceSpec) { s.Volumes[0].VolumeID = "" }, "volume_id"},
		{"relative mount", func(s *ServiceSpec) { s.Volumes[0].MountPath = "model" }, "absolute"},
		{"duplicate mount", func(s *ServiceSpec) { s.Volumes = append(s.Volumes, s.Volumes[0]) }, "used twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := modelSpec()
			tt.mod(&spec)
			_, err := BuildMicroservice(spec)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestBuildMicroserviceReportsAllProblems(t *testing.T) {
	_, err := BuildMicroservice(ServiceSpec{})
	if err == nil {
		t.Fatal("empty spec accepted")
	}
	for _, want := range []string{"name", "namespace", "image", "direct constraint", "memory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// TestCommandIsFullArgv pins the node engine contract: everything goes to
// "cmd" (which replaces ENTRYPOINT and CMD) and "args", which the node
// engine never reads, stays empty.
func TestCommandIsFullArgv(t *testing.T) {
	ms, err := BuildMicroservice(modelSpec())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms.Args) != 0 {
		t.Errorf("args = %q, want none", ms.Args)
	}
	if len(ms.Cmd) == 0 || ms.Cmd[0] != "vllm" {
		t.Errorf("cmd = %q, want the executable first", ms.Cmd)
	}
}

func TestHashedName(t *testing.T) {
	a := HashedName("qwen3", "k1", 8)
	if a != HashedName("qwen3", "k1", 8) {
		t.Error("not stable")
	}
	if a == HashedName("qwen3", "k2", 8) {
		t.Error("different keys collide")
	}
	if !ValidName(a) || len(a) != len("qwen3")+8 {
		t.Errorf("name %q", a)
	}
	long := HashedName("model-"+strings.Repeat("x", 40), "k", 8)
	if !ValidName(long) || len(long) != MaxNameLen {
		t.Errorf("long name %q (%d)", long, len(long))
	}
	if n := HashedName("q.w-e_n", "k", 1); !ValidName(n) || len(n) != len("qwen")+4 {
		t.Errorf("punctuation and clamped hash: %q", n)
	}
}

func TestEnvListSorted(t *testing.T) {
	got := EnvList(map[string]string{"B": "2", "A": "1=x"})
	if strings.Join(got, ",") != "A=1=x,B=2" {
		t.Errorf("EnvList = %q", got)
	}
	if EnvList(nil) != nil {
		t.Error("empty env not nil")
	}
}
