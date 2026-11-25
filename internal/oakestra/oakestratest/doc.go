// Package oakestratest is an in-memory fake of the Oakestra APIs the addon
// uses: the system manager's auth, cluster, application, service and
// instance endpoints, and one resource abstractor per cluster. It is an
// http.Handler, so tests serve it with httptest and cmd/ooa-fakeoak serves
// it on a real port.
//
// Behaviour follows the real components closely enough to exercise the
// client (JWT-shaped tokens with a 10 minute expiry, refresh tokens, 401 on
// expiry, 409 on duplicate applications, the 500 on deleting a missing
// service) while staying deterministic: ids are counters and deploying an
// instance places it synchronously on the node named by the direct
// constraint, with publicip set to that node's IP.
//
// Like the real scheduler, placement fails when the pinned node lacks a CSI
// driver one of the service's volumes names: the service gets the status
// NoActiveClusterWithCapacity (what the real scheduler reports) and a
// status_detail naming the driver, and no instance is created. Every node
// advertises HostpathCSIDriver and ModelpackCSIDriver unless
// NodeSpec.CSIDrivers says otherwise.
//
// Every cluster's abstractor is served under /cluster/{name}/api/v1/...
// (see AbstractorPath); the first cluster is also served at /api/v1/... so
// a single-cluster setup works with the default http://<cluster ip>:<port>
// URL scheme.
//
// The Go API (AddNode, UpdateNode, RemoveNode, Fail, ExpireTokens,
// Services, ...) lets tests drive and inspect the fake directly.
package oakestratest
