// Command ooa-fakeoak is a fake Oakestra (system manager plus one resource
// abstractor per cluster) with N simulated nodes, so the addon can be
// developed and demonstrated on a laptop. It serves
// internal/oakestra/oakestratest on one listener.
//
// Wiring the server to it: set OOA_OAKESTRA_API and
// OOA_CLUSTER_ABSTRACTOR_OVERRIDES to the values logged at start (pass
// -public-url when the server reaches the fake under another address, as in
// compose). The overrides point every cluster's abstractor at this listener
// under /cluster/<name>, for example
//
//	OOA_CLUSTER_ABSTRACTOR_OVERRIDES=cluster1=http://ooa-fakeoak:10000/cluster/cluster1
//
// Clusters report -cluster-ip, 127.0.0.254 by default, which nothing
// listens on; that is why the overrides are needed. Nodes report -node-ip,
// 127.0.0.1 by default, so engine host ports are reachable locally. In
// compose -node-ip is the fake's service name and -engine-bind 0.0.0.0, so
// the server reaches engines at http://ooa-fakeoak:<host port>. All nodes
// share that address, so host ports must be unique across nodes, which the
// server's port allocator guarantees per node IP.
//
// Every node advertises the hostpath and modelpack CSI drivers except the
// ones listed in -no-modelpack, which lack the modelpack driver; the fake
// scheduler refuses model services pinned to them, like the real one.
//
// Unless -spawn-engines=false, every deployed instance with a host port gets
// a small OpenAI-compatible engine built into this binary, listening on
// -engine-bind:<host port> and stopped on undeploy or node removal. It
// reads the SLA cmd like vLLM would: --served-model-name is the only model
// it serves and answers with, and --api-key (or VLLM_API_KEY in the SLA
// environment) is required as bearer on /v1/*, while /health stays open.
// It serves GET /health, GET /v1/models, POST /v1/chat/completions and
// /v1/completions (both with SSE streaming) and POST /v1/embeddings. A port
// that is already taken fails the instance, as a real container would.
//
// Every flag can also be set through the environment as OOA_FAKEOAK_<FLAG>
// with dashes turned into underscores (OOA_FAKEOAK_NODE_IP); flags win.
//
// The admin API under /__fake simulates the cluster changing:
//
//	GET    /__fake/nodes           list nodes as the abstractor reports them
//	POST   /__fake/nodes           add a node; body fields name, cluster, ip,
//	                               arch, memory_mb, used_mb, gpus, vram_mb,
//	                               csi_drivers are optional
//	PATCH  /__fake/nodes/{name}    change used_mb, memory_mb, active, csi_drivers
//	DELETE /__fake/nodes/{name}    remove a node; its instances fail
//	GET    /__fake/services        services with their instances
package main
