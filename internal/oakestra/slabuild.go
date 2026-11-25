package oakestra

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
)

// MaxNameLen is the longest application, namespace or microservice name
// the SLA schema accepts (^[a-zA-Z0-9]{1,32}$).
const MaxNameLen = 32

// ValidName reports whether s matches the SLA name pattern.
func ValidName(s string) bool {
	if s == "" || len(s) > MaxNameLen {
		return false
	}
	for _, r := range s {
		if !isAlnum(r) {
			return false
		}
	}
	return true
}

func isAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// HashedName builds a valid, stable microservice name from a readable
// prefix and a key: the prefix with every non-alphanumeric rune dropped,
// followed by the first hashLen hex digits of sha256(key). The prefix is
// shortened when needed so the result never exceeds MaxNameLen, and hashLen
// is clamped to [4, MaxNameLen-1].
func HashedName(prefix, key string, hashLen int) string {
	hashLen = min(max(hashLen, 4), MaxNameLen-1)
	var b strings.Builder
	for _, r := range prefix {
		if isAlnum(r) {
			b.WriteRune(r)
		}
	}
	p := b.String()
	if len(p) > MaxNameLen-hashLen {
		p = p[:MaxNameLen-hashLen]
	}
	sum := sha256.Sum256([]byte(key))
	return p + hex.EncodeToString(sum[:])[:hashLen]
}

// ServiceSpec is everything decided about one container pinned to a node.
type ServiceSpec struct {
	// Name and Namespace must match ^[a-zA-Z0-9]{1,32}$ (see HashedName).
	Name, Namespace string
	// Image is the container image.
	Image string
	// Command is the complete argv, executable first, rendered as SLA
	// "cmd". The node engine passes "cmd" to containerd's
	// oci.WithProcessArgs, which replaces the image's ENTRYPOINT and CMD
	// together, and never reads SLA "args"; so there is no separate args
	// field. Empty keeps the image's own entrypoint and command.
	Command []string
	// Env is rendered sorted as KEY=VALUE entries, so the SLA is
	// deterministic.
	Env map[string]string
	// HostPort publishes ContainerPort on the node as "host:container".
	// Zero publishes nothing.
	HostPort, ContainerPort int
	// MemoryMB must be positive; the scheduler compares it with the node's
	// available memory.
	MemoryMB int
	// VGPUs is the number of GPUs the container gets.
	VGPUs int
	// Cluster and Node are the candidate names of the target cluster and
	// node for the direct placement constraint. Both are required: the
	// addon never lets Oakestra pick a node.
	Cluster, Node string
	// Volumes are CSI volumes. The scheduler only picks a node that
	// advertises every driver named here, so a missing driver surfaces as
	// a scheduling failure, not as a container error.
	Volumes []Volume
}

// BuildMicroservice validates spec and renders the SLA microservice. Every
// problem is reported at once.
func BuildMicroservice(spec ServiceSpec) (Microservice, error) {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !ValidName(spec.Name) {
		bad("name %q must match ^[a-zA-Z0-9]{1,32}$", spec.Name)
	}
	if !ValidName(spec.Namespace) {
		bad("namespace %q must match ^[a-zA-Z0-9]{1,32}$", spec.Namespace)
	}
	if spec.Image == "" {
		bad("image is empty")
	}
	if len(spec.Command) > 0 && spec.Command[0] == "" {
		bad("command must start with the executable")
	}
	if spec.Cluster == "" || spec.Node == "" {
		bad("cluster and node are required for the direct constraint")
	}
	if spec.MemoryMB <= 0 {
		bad("memory must be positive")
	}
	if spec.VGPUs < 0 {
		bad("vgpus must not be negative")
	}
	if spec.HostPort != 0 && (!validPort(spec.HostPort) || !validPort(spec.ContainerPort)) {
		bad("ports %d:%d out of range", spec.HostPort, spec.ContainerPort)
	}
	for k := range spec.Env {
		if k == "" || strings.ContainsAny(k, "= \t\n") {
			bad("environment key %q is invalid", k)
		}
	}
	mounts := make(map[string]bool, len(spec.Volumes))
	for _, v := range spec.Volumes {
		if v.VolumeID == "" || v.CSIDriver == "" {
			bad("volume %q: volume_id and csi_driver are required", v.VolumeID)
		}
		if !path.IsAbs(v.MountPath) {
			bad("volume %q: mount path %q must be absolute", v.VolumeID, v.MountPath)
		}
		if mounts[v.MountPath] {
			bad("volume %q: mount path %q is used twice", v.VolumeID, v.MountPath)
		}
		mounts[v.MountPath] = true
	}
	if len(errs) > 0 {
		return Microservice{}, fmt.Errorf("oakestra: service %s: %w", spec.Name, errors.Join(errs...))
	}

	ms := Microservice{
		MicroserviceName:      spec.Name,
		MicroserviceNamespace: spec.Namespace,
		Virtualization:        VirtualizationContainer,
		Code:                  spec.Image,
		Memory:                spec.MemoryMB,
		VGPUs:                 spec.VGPUs,
		Cmd:                   slices.Clone(spec.Command),
		Environment:           EnvList(spec.Env),
		Constraints:           []Constraint{{Type: ConstraintDirect, Cluster: spec.Cluster, Node: spec.Node}},
	}
	if spec.HostPort != 0 {
		ms.Port = fmt.Sprintf("%d:%d", spec.HostPort, spec.ContainerPort)
	}
	for _, v := range spec.Volumes {
		v.Config = maps.Clone(v.Config)
		ms.Volumes = append(ms.Volumes, v)
	}
	return ms, nil
}

func validPort(p int) bool { return p > 0 && p <= 65535 }

// EnvList renders env as sorted KEY=VALUE entries, the SLA "environment"
// format. It returns nil for an empty map so the field is omitted.
func EnvList(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}

// NewApplication returns an application with no services; they are added
// one by one through CreateService.
func NewApplication(name, namespace, desc string) (Application, error) {
	if !ValidName(name) || !ValidName(namespace) {
		return Application{}, fmt.Errorf("oakestra: application %q.%q: names must match ^[a-zA-Z0-9]{1,32}$", name, namespace)
	}
	return Application{
		ApplicationName:      name,
		ApplicationNamespace: namespace,
		ApplicationDesc:      desc,
		Microservices:        []Microservice{},
	}, nil
}

// ApplicationSLA wraps an application into the SLA document POST
// /api/application/ expects.
func ApplicationSLA(customerID string, app Application) SLA {
	if app.Microservices == nil {
		app.Microservices = []Microservice{}
	}
	return SLA{SLAVersion: SLAVersion, CustomerID: customerID, Applications: []Application{app}}
}

// ServiceSLA wraps one microservice into the SLA document POST
// /api/service/ expects: the existing application's id, name and namespace
// (the schema requires all three) and the single new microservice.
func ServiceSLA(customerID string, app ApplicationInfo, ms Microservice) SLA {
	ms.MicroserviceID = ""
	return SLA{
		SLAVersion: SLAVersion,
		CustomerID: customerID,
		Applications: []Application{{
			ApplicationID:        app.ApplicationID,
			ApplicationName:      app.ApplicationName,
			ApplicationNamespace: app.ApplicationNamespace,
			ApplicationDesc:      app.ApplicationDesc,
			Microservices:        []Microservice{ms},
		}},
	}
}
