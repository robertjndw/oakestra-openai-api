package oakestra

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// ApplicationInfo is an application as GET /api/applications/ returns it:
// microservices are ids, not SLA objects.
type ApplicationInfo struct {
	ApplicationID        string   `json:"applicationID"`
	ApplicationName      string   `json:"application_name"`
	ApplicationNamespace string   `json:"application_namespace"`
	ApplicationDesc      string   `json:"application_desc"`
	Microservices        []string `json:"microservices"`
	UserID               string   `json:"userId"`
}

// CreateServiceResult is the body of POST /api/service/.
type CreateServiceResult struct {
	// JobID is the id of the last service created; with one microservice
	// per request (what CreateService sends) it is that service's id.
	JobID            string          `json:"job_id"`
	DeployedServices []ServiceResult `json:"deployed_services"`
	FailedServices   []ServiceResult `json:"failed_services"`
}

// ServiceResult is one entry of CreateServiceResult.
type ServiceResult struct {
	ServiceName string `json:"service_name"`
	Status      int    `json:"status"`
	Message     string `json:"message,omitempty"`
}

// Service is a service ("job") as GET /api/service/{id} returns it. Only
// fields the addon reads are declared.
type Service struct {
	ID                    string       `json:"_id"`
	MicroserviceID        string       `json:"microserviceID"`
	ApplicationID         string       `json:"applicationID"`
	AppName               string       `json:"app_name"`
	AppNS                 string       `json:"app_ns"`
	ServiceName           string       `json:"service_name"`
	ServiceNS             string       `json:"service_ns"`
	MicroserviceName      string       `json:"microservice_name"`
	MicroserviceNamespace string       `json:"microservice_namespace"`
	Image                 string       `json:"image"`
	Virtualization        string       `json:"virtualization"`
	Memory                int          `json:"memory"`
	VGPUs                 int          `json:"vgpus"`
	Port                  string       `json:"port"`
	Cmd                   []string     `json:"cmd"`
	Environment           []string     `json:"environment"`
	Constraints           []Constraint `json:"constraints"`
	Status                string       `json:"status"`
	StatusDetail          string       `json:"status_detail"`
	InstanceList          []Instance   `json:"instance_list"`
	NextInstanceNumber    int          `json:"next_instance_progressive_number"`
}

// Instance is one entry of Service.InstanceList.
type Instance struct {
	InstanceNumber  int       `json:"instance_number"`
	ClusterID       string    `json:"cluster_id"`
	ClusterLocation string    `json:"cluster_location"`
	Status          string    `json:"status"`
	StatusDetail    string    `json:"status_detail"`
	PublicIP        string    `json:"publicip"`
	WorkerID        string    `json:"worker_id"`
	HostIP          string    `json:"host_ip"`
	HostPort        FlexInt   `json:"host_port"`
	LastModified    Timestamp `json:"last_modified_timestamp"`
}

// Instance and service statuses (oakestra_utils/types/statuses.py).
const (
	StatusRequested        = "REQUESTED"
	StatusClusterScheduled = "CLUSTER_SCHEDULED"
	StatusNodeScheduled    = "NODE_SCHEDULED"
	StatusInstantiation    = "INSTANTIATION"
	StatusCreating         = "CREATING"
	StatusCreated          = "CREATED"
	StatusRunning          = "RUNNING"
	StatusFailed           = "FAILED"
	StatusDead             = "DEAD"
	StatusCompleted        = "COMPLETED"
	StatusUndeployed       = "UNDEPLOYED"
	StatusUnknown          = "UNKNOWN"
	// Negative scheduling outcomes.
	StatusNoWorkerCapacity       = "NO_WORKER_CAPACITY"
	StatusNoQualifiedWorkerFound = "NO_QUALIFIED_WORKER_FOUND"
	StatusNoNodeFound            = "NO_NODE_FOUND"
	StatusTargetClusterNotFound  = "TargetClusterNotFound"
	StatusTargetClusterNotActive = "TargetClusterNotActive"
	StatusTargetClusterNoCap     = "TargetClusterNoCapacity"
	StatusNoActiveClusterWithCap = "NoActiveClusterWithCapacity"
)

// Cluster is an entry of GET /api/clusters/. The system manager adds
// cluster_* aliases of the candidate fields; both are declared.
type Cluster struct {
	ID              string      `json:"_id"`
	Name            string      `json:"cluster_name"`
	CandidateName   string      `json:"candidate_name"`
	Location        string      `json:"cluster_location"`
	IP              string      `json:"ip"`
	Port            FlexString  `json:"port"`
	Active          bool        `json:"active"`
	ActiveNodes     int         `json:"active_nodes"`
	MemoryMB        float64     `json:"memory"`
	MemoryPercent   float64     `json:"memory_percent"`
	VCPUs           float64     `json:"vcpus"`
	VGPUs           float64     `json:"vgpus"`
	CPUPercent      float64     `json:"cpu_percent"`
	Virtualization  []string    `json:"virtualization"`
	CSIDrivers      []CSIDriver `json:"csi_drivers"`
	SupportedAddons []string    `json:"supported_addons"`
	LastModified    Timestamp   `json:"last_modified_timestamp"`
}

// DisplayName is the cluster's name as direct constraints expect it. The
// system manager fills cluster_name with "Unknown Cluster" when the
// candidate has no name, so that placeholder falls back to candidate_name.
func (cl Cluster) DisplayName() string {
	if cl.Name != "" && cl.Name != "Unknown Cluster" {
		return cl.Name
	}
	return cl.CandidateName
}

// NodeResource is a worker node as the cluster resource abstractor returns
// it from GET /api/v1/resources/. It is the node engine's Node report
// (go_node_engine/model/Node.go) plus abstractor fields.
type NodeResource struct {
	ID            string `json:"_id"`
	CandidateName string `json:"candidate_name"`
	Host          string `json:"host"`
	IP            string `json:"ip"`
	// Port is the node's net-manager port list (semicolon separated).
	Port         FlexString `json:"port"`
	Architecture string     `json:"architecture"`
	VCPUs        int        `json:"vcpus"`
	CPUPercent   float64    `json:"cpu_percent"`
	// MemoryMB is AVAILABLE memory in MiB (mem.Available >> 20).
	MemoryMB int `json:"memory"`
	// MemoryPercent is used memory in percent.
	MemoryPercent float64 `json:"memory_percent"`
	// VRAMMB is total GPU memory in MiB summed over devices; 0 when
	// nvidia-smi reports N/A (unified memory) or there is no GPU.
	VRAMMB          float64           `json:"vram"`
	VRAMPercent     float64           `json:"vram_percent"`
	VGPUs           int               `json:"vgpus"`
	GPUDriver       string            `json:"gpu_driver"`
	GPUUsage        float64           `json:"gpu_usage"`
	GPUTemp         float64           `json:"gpu_temp"`
	Virtualization  []string          `json:"virtualization"`
	SupportedAddons []string          `json:"supported_addons"`
	CSIDrivers      []CSIDriver       `json:"csi_drivers"`
	SystemInfo      map[string]string `json:"system_info"`
	// Active is computed by the abstractor from last_modified_timestamp.
	Active       bool      `json:"active"`
	LastModified Timestamp `json:"last_modified_timestamp"`
}

// NodeResourceFields is the ?resources= value that makes the abstractor
// include every field NodeResource declares beyond its canonical set.
// "active" must be listed too: the abstractor computes it with $addFields
// and then projects only the requested fields, so without it every node
// would decode as inactive.
const NodeResourceFields = "architecture,host,gpu_driver,gpu_usage,last_modified_timestamp,system_info,active"

// CSIDriver is a CSI driver entry. Nodes report objects
// {"csi_driver_name","csi_driver_endpoint"}; cluster aggregates report bare
// names. Both decode into Name.
type CSIDriver struct {
	Name     string `json:"csi_driver_name"`
	Endpoint string `json:"csi_driver_endpoint,omitempty"`
}

// UnmarshalJSON accepts a string or an object.
func (d *CSIDriver) UnmarshalJSON(b []byte) error {
	var name string
	if err := json.Unmarshal(b, &name); err == nil {
		*d = CSIDriver{Name: name}
		return nil
	}
	type plain CSIDriver
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("oakestra: csi driver: %w", err)
	}
	*d = CSIDriver(p)
	return nil
}

// Timestamp decodes the float unix-seconds timestamps Oakestra stores.
type Timestamp struct{ time.Time }

// UnmarshalJSON accepts a number (unix seconds, fractional) or null.
func (t *Timestamp) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		t.Time = time.Time{}
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("oakestra: timestamp: %w", err)
	}
	sec, frac := math.Modf(f)
	t.Time = time.Unix(int64(sec), int64(frac*1e9)).UTC()
	return nil
}

// MarshalJSON writes unix seconds, so fakes can serve the same shape.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(float64(t.UnixNano()) / 1e9)
}

// FlexString decodes a JSON string or number into a string. Oakestra
// documents some fields as strings but Python happily stores ints there.
type FlexString string

// UnmarshalJSON accepts a string, a number or null.
func (s *FlexString) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*s = ""
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		*s = FlexString(str)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("oakestra: expected string or number, got %s", b)
	}
	*s = FlexString(n.String())
	return nil
}

// FlexInt decodes a JSON number or numeric string into an int.
type FlexInt int

// UnmarshalJSON accepts a number, a numeric string, "" or null.
func (i *FlexInt) UnmarshalJSON(b []byte) error {
	var s FlexString
	if err := s.UnmarshalJSON(b); err != nil {
		return err
	}
	if s == "" {
		*i = 0
		return nil
	}
	n := json.Number(s)
	v, err := n.Int64()
	if err != nil {
		return fmt.Errorf("oakestra: expected integer, got %s", b)
	}
	*i = FlexInt(v)
	return nil
}
