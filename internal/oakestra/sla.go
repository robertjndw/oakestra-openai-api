package oakestra

// SLAVersion is the only SLA format version this package produces.
const SLAVersion = "v2.0"

// VirtualizationContainer is the SLA value for container services.
const VirtualizationContainer = "container"

// ConstraintDirect pins a service to a cluster and node by name.
const ConstraintDirect = "direct"

// HostpathCSIDriver is Oakestra's CSI driver for node-local volumes.
const HostpathCSIDriver = "csi.oakestra.io/hostpath"

// ModelpackCSIDriver is the upstream ModelPack CSI driver
// (github.com/modelpack/model-csi-driver), and ModelpackReferenceKey the
// volume attribute that names the OCI model to mount. They are the defaults
// of OOA_MODEL_CSI_DRIVER and OOA_MODEL_CSI_REF_KEY. The driver treats a
// volume id starting with "pvc-" as a dynamically provisioned volume, so
// static model volumes must never use that prefix.
const (
	ModelpackCSIDriver    = "model.csi.modelpack.org"
	ModelpackReferenceKey = "model.csi.modelpack.org/reference"
)

// SLA is the top-level deployment descriptor accepted by
// POST /api/application/ and POST /api/service/.
type SLA struct {
	SLAVersion   string        `json:"sla_version"`
	CustomerID   string        `json:"customerID"`
	Applications []Application `json:"applications"`
	Args         []string      `json:"args,omitempty"`
}

// Application is one application inside an SLA.
type Application struct {
	// ApplicationID is required when adding services to an existing
	// application and omitted when creating one.
	ApplicationID        string         `json:"applicationID,omitempty"`
	ApplicationName      string         `json:"application_name"`
	ApplicationNamespace string         `json:"application_namespace"`
	ApplicationDesc      string         `json:"application_desc,omitempty"`
	Microservices        []Microservice `json:"microservices"`
}

// Microservice is one service in an SLA. Field names and types follow
// sla/schema.py exactly; omitempty keeps generated SLAs minimal, which also
// keeps golden files readable.
type Microservice struct {
	// MicroserviceID must be empty in requests (schema maxLength 0).
	MicroserviceID        string `json:"microserviceID,omitempty"`
	MicroserviceName      string `json:"microservice_name"`
	MicroserviceNamespace string `json:"microservice_namespace"`
	// Virtualization is VirtualizationContainer for everything we deploy.
	Virtualization string `json:"virtualization"`
	// Code is the container image.
	Code string `json:"code"`
	// Memory is in MB. The scheduler only checks it against the node's
	// available memory; nothing enforces it at runtime.
	Memory       int    `json:"memory,omitempty"`
	VCPUs        int    `json:"vcpus,omitempty"`
	VGPUs        int    `json:"vgpus,omitempty"`
	VTPUs        int    `json:"vtpus,omitempty"`
	BandwidthIn  int    `json:"bandwidth_in,omitempty"`
	BandwidthOut int    `json:"bandwidth_out,omitempty"`
	Storage      int    `json:"storage,omitempty"`
	State        string `json:"state,omitempty"`
	// Port publishes container ports, "hostPort:containerPort" (several
	// mappings separated by ";", optional "/tcp" or "/udp" suffix).
	Port       string `json:"port,omitempty"`
	OneShot    bool   `json:"one_shot,omitempty"`
	Privileged bool   `json:"privileged,omitempty"`
	// Cmd is the container's complete argv, executable first. Oakestra's
	// node engine hands it to containerd's oci.WithProcessArgs, which
	// replaces the image's ENTRYPOINT and CMD together.
	Cmd []string `json:"cmd,omitempty"`
	// Args is accepted by the SLA schema but the node engine never reads
	// it; BuildMicroservice leaves it empty and puts every argument in Cmd.
	Args []string `json:"args,omitempty"`
	// Environment entries are "KEY=VALUE".
	Environment          []string       `json:"environment,omitempty"`
	SLAViolationStrategy string         `json:"sla_violation_strategy,omitempty"`
	TargetNode           string         `json:"target_node,omitempty"`
	Addresses            *Addresses     `json:"addresses,omitempty"`
	AddedFiles           []string       `json:"added_files,omitempty"`
	Volumes              []Volume       `json:"volumes,omitempty"`
	Constraints          []Constraint   `json:"constraints,omitempty"`
	Connectivity         []Connectivity `json:"connectivity,omitempty"`
}

// Volume is a CSI volume request. The node engine stages and publishes it
// through the named driver and bind-mounts it at MountPath.
type Volume struct {
	// VolumeID identifies the volume to the driver. Its meaning is
	// driver-specific: the hostpath driver takes the absolute host path,
	// the modelpack driver any id unique per mount.
	VolumeID string `json:"volume_id"`
	// CSIDriver must equal the name the driver registered on the node.
	CSIDriver string `json:"csi_driver"`
	MountPath string `json:"mount_path,omitempty"`
	// Config is passed to the driver as VolumeContext.
	Config map[string]string `json:"config,omitempty"`
}

// Constraint is a placement constraint. For Type == ConstraintDirect set
// Cluster and Node to candidate names.
type Constraint struct {
	Type            string   `json:"type"`
	Area            string   `json:"area,omitempty"`
	Cluster         string   `json:"cluster,omitempty"`
	Node            string   `json:"node,omitempty"`
	Location        string   `json:"location,omitempty"`
	Threshold       float64  `json:"threshold,omitempty"`
	Rigidness       float64  `json:"rigidness,omitempty"`
	ConvergenceTime int      `json:"convergence_time,omitempty"`
	Needs           []string `json:"needs,omitempty"`
	Allowed         []string `json:"allowed,omitempty"`
}

// Addresses requests fixed service IPs.
type Addresses struct {
	RRIP        string            `json:"rr_ip,omitempty"`
	RRIPv6      string            `json:"rr_ip_v6,omitempty"`
	ClosestIP   string            `json:"closest_ip,omitempty"`
	ClosestIPv6 string            `json:"closest_ip_v6,omitempty"`
	Instances   []AddressInstance `json:"instances,omitempty"`
}

// AddressInstance is one entry of Addresses.Instances.
type AddressInstance struct {
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	Start string `json:"start,omitempty"`
}

// Connectivity is a network constraint towards another microservice.
type Connectivity struct {
	TargetMicroserviceID string                   `json:"target_microservice_id"`
	ConConstraints       []ConnectivityConstraint `json:"con_constraints"`
}

// ConnectivityConstraint is one entry of Connectivity.ConConstraints.
type ConnectivityConstraint struct {
	Type            string  `json:"type"`
	Threshold       float64 `json:"threshold"`
	Rigidness       float64 `json:"rigidness"`
	ConvergenceTime int     `json:"convergence_time"`
}
