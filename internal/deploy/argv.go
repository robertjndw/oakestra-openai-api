package deploy

import (
	"strconv"
	"strings"
)

// EngineArgv returns the complete container argv for model m. The SLA cmd
// replaces the image's ENTRYPOINT and CMD together, so the executable must
// be part of it.
//
// Without a command template it is the vLLM server:
//
//	vllm serve <modelPath> --served-model-name <name> --port <port> --api-key <engineKey>
//
// The served name equals the public model name, so the gateway never has to
// rewrite "model" in responses. With a template, the placeholders
// {model_path}, {name}, {port} and {api_key} are substituted in every
// element. engine_args are appended verbatim in both cases.
func EngineArgv(m Model, modelPath, engineKey string) []string {
	port := strconv.Itoa(m.ContainerPort)
	var argv []string
	if len(m.Command) == 0 {
		argv = []string{"vllm", "serve", modelPath,
			"--served-model-name", m.Name,
			"--port", port,
			"--api-key", engineKey}
	} else {
		r := strings.NewReplacer("{model_path}", modelPath, "{name}", m.Name, "{port}", port, "{api_key}", engineKey)
		argv = make([]string, 0, len(m.Command)+len(m.EngineArgs))
		for _, a := range m.Command {
			argv = append(argv, r.Replace(a))
		}
	}
	return append(argv, m.EngineArgs...)
}
