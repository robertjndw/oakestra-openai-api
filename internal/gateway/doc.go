// Package gateway is the public OpenAI-compatible listener. It authenticates
// API keys, picks a ready engine for the requested model (or for "auto",
// through a scheduler.Strategy), rewrites the model name, and streams the
// engine's answer back unchanged.
//
// The gateway knows nothing about how deployments are created: whoever owns
// the lifecycle pushes the routable set with SetEndpoints whenever it
// changes.
package gateway
