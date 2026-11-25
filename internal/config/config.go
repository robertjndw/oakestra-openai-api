package config

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// AdminAuthMode selects how the admin API authenticates.
type AdminAuthMode string

const (
	// AdminAuthIntrospect validates the dashboard token by calling
	// GET {OOA_AUTH_URL}/user/<sub> with it.
	AdminAuthIntrospect AdminAuthMode = "introspect"
	// AdminAuthNone treats every admin request as an Admin. Tests only.
	AdminAuthNone AdminAuthMode = "none"
)

// Config is the server's configuration.
type Config struct {
	// Listen is the public listener (/v1, /healthz).
	Listen string `env:"OOA_LISTEN" envDefault:":8080"`
	// AdminListen is the admin listener (/admin/v1). Only the UI container
	// should be able to reach it.
	AdminListen string `env:"OOA_ADMIN_LISTEN" envDefault:":8081"`
	// PublicURL is the base URL external clients use for /v1, shown in the
	// UI. Optional: the UI falls back to its own origin.
	PublicURL string `env:"OOA_PUBLIC_URL"`
	// DataDir holds the SQLite database.
	DataDir string `env:"OOA_DATA_DIR" envDefault:"/data"`
	// LogLevel is debug, info, warn or error.
	LogLevel slog.Level `env:"OOA_LOG_LEVEL" envDefault:"info"`

	Oakestra Oakestra

	// ClusterAbstractorPort is the port of each cluster's resource
	// abstractor, reached at the cluster IP from /api/clusters/.
	ClusterAbstractorPort int `env:"OOA_CLUSTER_ABSTRACTOR_PORT" envDefault:"11012"`
	// ClusterAbstractorOverrides maps a cluster name (or id) to the base URL
	// of its resource abstractor, for clusters whose advertised IP is not
	// reachable from here. Format: "name=url,name2=url2".
	ClusterAbstractorOverrides map[string]string `env:"OOA_CLUSTER_ABSTRACTOR_OVERRIDES" envKeyValSeparator:"="`

	// AppName and AppNS name the single Oakestra application that owns every
	// engine service. Both must match ^[a-zA-Z0-9]{1,32}$.
	AppName string `env:"OOA_APP_NAME" envDefault:"openaiapi"`
	AppNS   string `env:"OOA_APP_NS" envDefault:"ooa"`

	// EnginePortRange is the host port range engines are published on.
	EnginePortRange PortRange `env:"OOA_ENGINE_PORT_RANGE" envDefault:"18000-18999"`
	// DefaultEngineImage is prefilled for new models. The tag is pinned
	// because the default engine argv is written against that vLLM's flags.
	DefaultEngineImage string `env:"OOA_DEFAULT_ENGINE_IMAGE" envDefault:"vllm/vllm-openai:v0.29.0"`

	// ModelCSIDriver and ModelCSIRefKey are the CSI driver name and the
	// volume attribute that carries the OCI model reference. They must match
	// what the driver on the nodes registers, since the node engine looks
	// the driver up by that exact name.
	ModelCSIDriver string `env:"OOA_MODEL_CSI_DRIVER" envDefault:"model.csi.modelpack.org"`
	ModelCSIRefKey string `env:"OOA_MODEL_CSI_REF_KEY" envDefault:"model.csi.modelpack.org/reference"`
	// ModelMountPath is where the model volume appears inside the engine.
	ModelMountPath string `env:"OOA_MODEL_MOUNT_PATH" envDefault:"/model"`

	// PollInterval is the period of the deployment loop.
	PollInterval time.Duration `env:"OOA_POLL_INTERVAL" envDefault:"5s"`
	// ScheduleTimeout bounds the wait for Oakestra to report RUNNING. It
	// includes the model pull done by the CSI driver, hence the size.
	ScheduleTimeout time.Duration `env:"OOA_SCHEDULE_TIMEOUT" envDefault:"15m"`
	// StartupTimeout bounds the wait for the engine to answer its health
	// probe after the container runs (weights loading, compilation).
	StartupTimeout time.Duration `env:"OOA_STARTUP_TIMEOUT" envDefault:"30m"`

	// Scheduler seeds the routing strategy setting on first start.
	Scheduler string `env:"OOA_SCHEDULER" envDefault:"least-inflight"`
	// MaxAttempts is how many candidates the gateway tries for one request
	// when connecting fails before any response.
	MaxAttempts int `env:"OOA_MAX_ATTEMPTS" envDefault:"3"`
	// MaxBodyBytes caps /v1 request bodies. Accepts a plain byte count or a
	// KiB/MiB/GiB suffix.
	MaxBodyBytes ByteSize `env:"OOA_MAX_BODY_BYTES" envDefault:"16MiB"`
	// DebugHeaders adds deployment and node headers to /v1 responses.
	DebugHeaders bool `env:"OOA_DEBUG_HEADERS" envDefault:"false"`

	// AuthURL is the system-manager API base used to introspect dashboard
	// tokens (GET {AuthURL}/user/<sub>).
	AuthURL string `env:"OOA_AUTH_URL" envDefault:"http://system_manager:10000/api"`
	// AdminAuth is introspect (default) or none (tests only).
	AdminAuth AdminAuthMode `env:"OOA_ADMIN_AUTH" envDefault:"introspect"`
}

// Oakestra holds the service-account connection to the root orchestrator.
// It is a service account because the deployment loop runs without a user
// and dashboard tokens expire after 10 minutes.
type Oakestra struct {
	// API is the system-manager base URL.
	API      string `env:"OOA_OAKESTRA_API" envDefault:"http://system_manager:10000"`
	User     string `env:"OOA_OAKESTRA_USER"`
	Password string `env:"OOA_OAKESTRA_PASSWORD"`
	// Org is the organization name sent at login; empty means root.
	Org string `env:"OOA_OAKESTRA_ORG"`
}

// Load parses the process environment and validates the result.
func Load() (Config, error) {
	return parse(env.Options{})
}

// LoadFrom is Load over an explicit environment, for tests and tools.
func LoadFrom(environ map[string]string) (Config, error) {
	return parse(env.Options{Environment: environ})
}

func parse(opts env.Options) (Config, error) {
	var c Config
	if err := env.ParseWithOptions(&c, opts); err != nil {
		return Config{}, fmt.Errorf("config: %w", explainParseError(err, reflect.TypeFor[Config]()))
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate reports every problem at once, so an operator fixes the
// environment in one round.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Listen == "" {
		add("OOA_LISTEN is empty")
	}
	if c.AdminListen == "" {
		add("OOA_ADMIN_LISTEN is empty")
	}
	if c.Listen != "" && c.Listen == c.AdminListen {
		add("OOA_LISTEN and OOA_ADMIN_LISTEN must differ: the admin API must not be reachable on the public port")
	}
	if c.PublicURL != "" {
		if err := checkHTTPURL(c.PublicURL); err != nil {
			add("OOA_PUBLIC_URL: %v", err)
		}
	}
	if c.DataDir == "" {
		add("OOA_DATA_DIR is empty")
	}
	if err := checkHTTPURL(c.Oakestra.API); err != nil {
		add("OOA_OAKESTRA_API: %v", err)
	}
	if c.Oakestra.User == "" {
		add("OOA_OAKESTRA_USER is required")
	}
	if c.Oakestra.Password == "" {
		add("OOA_OAKESTRA_PASSWORD is required")
	}
	if c.ClusterAbstractorPort <= 0 || c.ClusterAbstractorPort > 65535 {
		add("OOA_CLUSTER_ABSTRACTOR_PORT %d is not a port", c.ClusterAbstractorPort)
	}
	for name, u := range c.ClusterAbstractorOverrides {
		if name == "" {
			add("OOA_CLUSTER_ABSTRACTOR_OVERRIDES: empty cluster name")
		}
		if err := checkHTTPURL(u); err != nil {
			add("OOA_CLUSTER_ABSTRACTOR_OVERRIDES[%s]: %v", name, err)
		}
	}
	if !validOakestraName(c.AppName) {
		add("OOA_APP_NAME %q must match ^[a-zA-Z0-9]{1,32}$", c.AppName)
	}
	if !validOakestraName(c.AppNS) {
		add("OOA_APP_NS %q must match ^[a-zA-Z0-9]{1,32}$", c.AppNS)
	}
	if c.EnginePortRange.Size() == 0 {
		add("OOA_ENGINE_PORT_RANGE is empty")
	}
	if c.DefaultEngineImage == "" {
		add("OOA_DEFAULT_ENGINE_IMAGE is empty")
	}
	if c.ModelCSIDriver == "" {
		add("OOA_MODEL_CSI_DRIVER is empty")
	}
	if c.ModelCSIRefKey == "" {
		add("OOA_MODEL_CSI_REF_KEY is empty")
	}
	if !strings.HasPrefix(c.ModelMountPath, "/") {
		add("OOA_MODEL_MOUNT_PATH %q must be an absolute path", c.ModelMountPath)
	}
	if c.PollInterval < 100*time.Millisecond {
		add("OOA_POLL_INTERVAL %s is below 100ms", c.PollInterval)
	}
	if c.ScheduleTimeout <= 0 {
		add("OOA_SCHEDULE_TIMEOUT must be positive")
	}
	if c.StartupTimeout <= 0 {
		add("OOA_STARTUP_TIMEOUT must be positive")
	}
	if c.Scheduler == "" {
		add("OOA_SCHEDULER is empty")
	}
	if c.MaxAttempts < 1 {
		add("OOA_MAX_ATTEMPTS must be at least 1")
	}
	if c.MaxBodyBytes <= 0 {
		add("OOA_MAX_BODY_BYTES must be positive")
	}
	switch c.AdminAuth {
	case AdminAuthIntrospect:
		if err := checkHTTPURL(c.AuthURL); err != nil {
			add("OOA_AUTH_URL: %v", err)
		}
	case AdminAuthNone:
	default:
		add("OOA_ADMIN_AUTH must be introspect or none, got %q", c.AdminAuth)
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("config: %w", errors.Join(errs...))
}

// Warnings lists settings that are valid but risky, for logging at start.
func (c Config) Warnings() []string {
	var w []string
	if c.AdminAuth == AdminAuthNone {
		w = append(w, "OOA_ADMIN_AUTH=none: every admin request is treated as an Admin; use only for tests")
	}
	if c.Oakestra.User == "Admin" && c.Oakestra.Password == "Admin" {
		w = append(w, "OOA_OAKESTRA_USER/PASSWORD are the Admin/Admin defaults; create a dedicated Oakestra user")
	}
	if c.DebugHeaders {
		w = append(w, "OOA_DEBUG_HEADERS=true: /v1 responses reveal deployment and node names")
	}
	return w
}

// AbstractorURL returns the base URL of a cluster's resource abstractor:
// an override by cluster name or id if configured, else
// http://<clusterIP>:<OOA_CLUSTER_ABSTRACTOR_PORT>. The result has no
// trailing slash.
func (c Config) AbstractorURL(clusterName, clusterID, clusterIP string) string {
	for _, k := range []string{clusterName, clusterID} {
		if u, ok := c.ClusterAbstractorOverrides[k]; ok && k != "" {
			return strings.TrimRight(u, "/")
		}
	}
	return "http://" + net.JoinHostPort(clusterIP, strconv.Itoa(c.ClusterAbstractorPort))
}

// PortRange is an inclusive range of TCP ports, written "lo-hi".
type PortRange struct {
	Lo, Hi int
}

// UnmarshalText parses "lo-hi". It lets env parse the field directly.
func (p *PortRange) UnmarshalText(b []byte) error {
	lo, hi, ok := strings.Cut(strings.TrimSpace(string(b)), "-")
	if !ok {
		return fmt.Errorf("port range %q is not lo-hi", b)
	}
	l, err1 := strconv.Atoi(strings.TrimSpace(lo))
	h, err2 := strconv.Atoi(strings.TrimSpace(hi))
	if err1 != nil || err2 != nil {
		return fmt.Errorf("port range %q is not numeric", b)
	}
	if l < 1 || h > 65535 || l > h {
		return fmt.Errorf("port range %q must satisfy 1 <= lo <= hi <= 65535", b)
	}
	p.Lo, p.Hi = l, h
	return nil
}

// String formats the range as "lo-hi".
func (p PortRange) String() string { return fmt.Sprintf("%d-%d", p.Lo, p.Hi) }

// Contains reports whether port is inside the range.
func (p PortRange) Contains(port int) bool { return port >= p.Lo && port <= p.Hi }

// Size is the number of ports in the range.
func (p PortRange) Size() int {
	if p.Hi < p.Lo || p.Lo == 0 {
		return 0
	}
	return p.Hi - p.Lo + 1
}

// ByteSize is a size in bytes, written as a plain integer or with a KiB,
// MiB or GiB suffix ("16MiB").
type ByteSize int64

// UnmarshalText parses the size.
func (s *ByteSize) UnmarshalText(b []byte) error {
	str := strings.TrimSpace(string(b))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}} {
		if v, ok := strings.CutSuffix(str, u.suffix); ok {
			str, mult = strings.TrimSpace(v), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(str, 10, 64)
	if err != nil || n < 0 {
		return fmt.Errorf("size %q is not a byte count (examples: 1048576, 16MiB)", b)
	}
	*s = ByteSize(n * mult)
	return nil
}

func checkHTTPURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q must be an http(s) URL", s)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", s)
	}
	return nil
}

// validOakestraName mirrors the SLA schema pattern ^[a-zA-Z0-9]{1,32}$. It
// is duplicated here so config does not depend on the oakestra package.
func validOakestraName(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// explainParseError rewrites env's parse errors, which name the Go field
// ("PollInterval"), to name the variable the operator actually sets.
func explainParseError(err error, t reflect.Type) error {
	var agg env.AggregateError
	if !errors.As(err, &agg) {
		return err
	}
	keys := envKeys(t)
	out := make([]error, 0, len(agg.Errors))
	for _, e := range agg.Errors {
		if pe, ok := errors.AsType[env.ParseError](e); ok {
			if key, ok := keys[pe.Name]; ok {
				out = append(out, fmt.Errorf("%s: %w", key, pe.Err))
				continue
			}
		}
		out = append(out, e)
	}
	return errors.Join(out...)
}

// envKeys maps Go field names to their env variable names, descending into
// nested structs. Field names are unique across Config and its nested
// structs, which keeps this flat map unambiguous.
func envKeys(t reflect.Type) map[string]string {
	keys := make(map[string]string)
	for f := range t.Fields() {
		if f.Type.Kind() == reflect.Struct && f.Tag.Get("env") == "" {
			maps.Copy(keys, envKeys(f.Type))
			continue
		}
		if tag, _, _ := strings.Cut(f.Tag.Get("env"), ","); tag != "" {
			keys[f.Name] = tag
		}
	}
	return keys
}
