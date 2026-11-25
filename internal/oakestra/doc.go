// Package oakestra talks to an Oakestra deployment: the root system
// manager (login, clusters, applications, services, instances) and each
// cluster's resource abstractor (nodes). Nothing outside this package
// builds HTTP requests to Oakestra.
//
// The Go types mirror the real JSON, checked against the Oakestra sources:
//   - SLA v2: root_orchestrator/system-manager-python/sla/schema.py
//   - node report: go_node_engine/model/Node.go (+ config.CSIDriverType)
//   - cluster list: system-manager blueprints/clusters_blueprints.py
//   - resources: resource-abstractor/api/v1/resources_blueprint.py
//
// Wire facts the client implementation must honour:
//   - Login is POST /api/auth/login with {"username","password",
//     "organization_name"} (organization_name omitted for root) and returns
//     {"token","refresh_token"}. Refresh is POST /api/auth/refresh with the
//     refresh token as bearer and returns {"token"}.
//   - POST /api/application/ takes a full SLA and returns the user's
//     applications (not just the new one); find the new one by name and
//     namespace. 409 means it exists already (see EnsureApplication).
//   - POST /api/service/ takes an SLA whose single application carries
//     applicationID and the microservices to add; the response has
//     {"job_id", "deployed_services", "failed_services"} and is a 200 even
//     when a service failed.
//   - The system manager does not list worker nodes. The resource
//     abstractor's GET /api/v1/resources/ does, but projects only a
//     canonical field set; pass ?resources=NodeResourceFields to get the
//     rest, including the computed "active" flag.
//   - Listing all applications needs the ADMIN role; a service account
//     lists its own with GET /api/user/{name} (for its _id) and then
//     GET /api/applications/{_id}.
//   - Deleting a missing service answers 500, not 404.
//   - Direct placement constraints match candidate_name: "cluster" is the
//     cluster's name, "node" the node's name (hostname), not their ids.
//   - The SLA "virtualization" value is "container"; Oakestra stores it as
//     "docker".
//   - SLA "cmd" replaces the image's ENTRYPOINT and CMD; "args" is ignored
//     by the node engine.
//   - A volume's "config" map reaches the CSI driver as VolumeContext, and
//     the scheduler only places a service on a node that advertises every
//     csi_driver its volumes name.
//
// HTTPClient (httpclient.go, auth.go) implements Client; the oakestratest
// fake serves the same wire format. Access tokens live 10 minutes: the
// client reads their exp claim, refreshes a minute early, lets concurrent
// callers share one login, and retries a request once after a 401.
// BuildMicroservice and the *SLA helpers (slabuild.go) produce the SLA
// documents.
package oakestra
