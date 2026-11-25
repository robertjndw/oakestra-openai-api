package deploy

import (
	"slices"
	"testing"
)

func TestEngineArgv(t *testing.T) {
	tests := []struct {
		name  string
		model Model
		want  []string
	}{
		{
			name:  "default vllm",
			model: Model{Name: "qwen", ContainerPort: 8000, EngineArgs: []string{}},
			want:  []string{"vllm", "serve", "/model", "--served-model-name", "qwen", "--port", "8000", "--api-key", "KEY"},
		},
		{
			name:  "default vllm with engine args",
			model: Model{Name: "qwen", ContainerPort: 9000, EngineArgs: []string{"--max-model-len", "8192"}},
			want: []string{"vllm", "serve", "/model", "--served-model-name", "qwen", "--port", "9000", "--api-key", "KEY",
				"--max-model-len", "8192"},
		},
		{
			name: "template",
			model: Model{Name: "llama", ContainerPort: 8080, EngineArgs: []string{"--ctx-size", "4096"},
				Command: []string{"llama-server", "-m", "{model_path}/model.gguf", "--alias", "{name}", "--port", "{port}", "--api-key={api_key}"}},
			want: []string{"llama-server", "-m", "/model/model.gguf", "--alias", "llama", "--port", "8080", "--api-key=KEY",
				"--ctx-size", "4096"},
		},
		{
			name:  "template without placeholders",
			model: Model{Name: "x", ContainerPort: 1, Command: []string{"serve", "{unknown}"}},
			want:  []string{"serve", "{unknown}"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := slices.Clone(tt.model.Command)
			got := EngineArgv(tt.model, "/model", "KEY")
			if !slices.Equal(got, tt.want) {
				t.Errorf("EngineArgv = %q\nwant          %q", got, tt.want)
			}
			if !slices.Equal(tt.model.Command, cmd) {
				t.Errorf("EngineArgv modified the model's command: %q", tt.model.Command)
			}
		})
	}
}
