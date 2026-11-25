package deploy

import (
	"net/http"
	"testing"
)

func TestCreateModelDefaults(t *testing.T) {
	h := newHarness(t, nil)
	mod, err := h.m.CreateModel(t.Context(), ModelInput{Name: "qwen", Reference: " registry.example/qwen:1 ", MemoryMB: 2048})
	if err != nil {
		t.Fatal(err)
	}
	if mod.EngineImage != h.cfg.DefaultEngineImage || mod.ContainerPort != 8000 || !mod.Auto ||
		mod.EngineArgs == nil || mod.Labels == nil || mod.Command != nil || mod.Reference != "registry.example/qwen:1" {
		t.Errorf("defaults not applied: %+v", mod)
	}
	got, err := h.m.GetModel(t.Context(), mod.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EngineImage != h.cfg.DefaultEngineImage || got.EngineArgs == nil || got.Labels == nil || got.Command != nil || !got.Auto {
		t.Errorf("stored model %+v", got)
	}
	if byName, err := h.m.GetModelByName(t.Context(), "qwen"); err != nil || byName.ID != mod.ID {
		t.Errorf("GetModelByName = %+v, %v", byName, err)
	}

	off := false
	custom, err := h.m.CreateModel(t.Context(), ModelInput{Name: "llama.cpp-1_b", Reference: "r", MemoryMB: 1,
		Command: []string{"llama-server", "-m", "{model_path}"}, Auto: &off, Labels: map[string]string{"tier": "small"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := h.m.GetModel(t.Context(), custom.ID); got.Auto || len(got.Command) != 3 || got.Labels["tier"] != "small" {
		t.Errorf("custom model %+v", got)
	}
	list, err := h.m.ListModels(t.Context())
	if err != nil || len(list) != 2 || list[0].Name != "llama.cpp-1_b" {
		t.Errorf("ListModels = %+v, %v", list, err)
	}
}

func TestCreateModelValidation(t *testing.T) {
	h := newHarness(t, nil)
	tests := []struct {
		name   string
		in     ModelInput
		status int
	}{
		{"auto reserved", ModelInput{Name: "auto", Reference: "r", MemoryMB: 1}, http.StatusUnprocessableEntity},
		{"AUTO reserved", ModelInput{Name: "AUTO", Reference: "r", MemoryMB: 1}, http.StatusUnprocessableEntity},
		{"bad name", ModelInput{Name: "a/b", Reference: "r", MemoryMB: 1}, http.StatusUnprocessableEntity},
		{"empty name", ModelInput{Reference: "r", MemoryMB: 1}, http.StatusUnprocessableEntity},
		{"long name", ModelInput{Name: string(make([]byte, 65)), Reference: "r", MemoryMB: 1}, http.StatusUnprocessableEntity},
		{"no reference", ModelInput{Name: "a", MemoryMB: 1}, http.StatusUnprocessableEntity},
		{"no memory", ModelInput{Name: "a", Reference: "r"}, http.StatusUnprocessableEntity},
		{"negative vgpus", ModelInput{Name: "a", Reference: "r", MemoryMB: 1, VGPUs: -1}, http.StatusUnprocessableEntity},
		{"bad port", ModelInput{Name: "a", Reference: "r", MemoryMB: 1, ContainerPort: 70000}, http.StatusUnprocessableEntity},
		{"empty executable", ModelInput{Name: "a", Reference: "r", MemoryMB: 1, Command: []string{""}}, http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.m.CreateModel(t.Context(), tt.in)
			assertStatus(t, err, tt.status)
		})
	}

	h.model("qwen")
	_, err := h.m.CreateModel(t.Context(), ModelInput{Name: "qwen", Reference: "r", MemoryMB: 1})
	assertStatus(t, err, http.StatusConflict)
	_, err = h.m.GetModel(t.Context(), "m_missing")
	assertStatus(t, err, http.StatusNotFound)
	assertStatus(t, h.m.DeleteModel(t.Context(), "m_missing"), http.StatusNotFound)
	_, err = h.m.UpdateModel(t.Context(), "m_missing", ModelPatch{Auto: new(false)})
	assertStatus(t, err, http.StatusNotFound)
}

func TestUpdateModel(t *testing.T) {
	h := newHarness(t, nil)
	mod := h.model("qwen")
	other := h.model("llama")

	up, err := h.m.UpdateModel(t.Context(), mod.ID, ModelPatch{
		MemoryMB: new(4096), Command: &[]string{"x", "{port}"}, EngineImage: new("img:2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if up.MemoryMB != 4096 || len(up.Command) != 2 || up.EngineImage != "img:2" || up.Name != "qwen" {
		t.Errorf("updated %+v", up)
	}
	up, err = h.m.UpdateModel(t.Context(), mod.ID, ModelPatch{Command: &[]string{}, EngineImage: new("")})
	if err != nil {
		t.Fatal(err)
	}
	if up.Command != nil || up.EngineImage != h.cfg.DefaultEngineImage {
		t.Errorf("reset to defaults failed: %+v", up)
	}
	_, err = h.m.UpdateModel(t.Context(), mod.ID, ModelPatch{Name: new("llama")})
	assertStatus(t, err, http.StatusConflict)
	_, err = h.m.UpdateModel(t.Context(), mod.ID, ModelPatch{Name: new("auto")})
	assertStatus(t, err, http.StatusUnprocessableEntity)
	if err := h.m.DeleteModel(t.Context(), other.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDeployedModelIsLocked(t *testing.T) {
	h := newHarness(t, nil)
	mod := h.model("qwen")
	h.deploy(mod, "c1", "n1")

	for name, p := range map[string]ModelPatch{
		"memory":    {MemoryMB: new(1)},
		"name":      {Name: new("other")},
		"reference": {Reference: new("registry.example/other:2")},
		"image":     {EngineImage: new("img:2")},
		"args":      {EngineArgs: &[]string{"--x"}},
		"command":   {Command: &[]string{"run"}},
		"port":      {ContainerPort: new(9000)},
		"vgpus":     {VGPUs: new(2)},
	} {
		_, err := h.m.UpdateModel(t.Context(), mod.ID, p)
		if !wantStatus(err, http.StatusConflict) {
			t.Errorf("%s: err = %v, want 409", name, err)
		}
	}

	// A full form that repeats the current values plus new auto and labels.
	up, err := h.m.UpdateModel(t.Context(), mod.ID, ModelPatch{
		Name: new(mod.Name), Reference: new(mod.Reference), MemoryMB: new(mod.MemoryMB),
		EngineArgs: &[]string{}, Auto: new(false), Labels: &map[string]string{"role": "coder"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if up.Auto || up.Labels["role"] != "coder" {
		t.Errorf("auto and labels not updated: %+v", up)
	}
	assertStatus(t, h.m.DeleteModel(t.Context(), mod.ID), http.StatusConflict)
}
