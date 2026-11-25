package oakestratest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/oakestra"
)

// Options configures a Fake. The zero value works.
type Options struct {
	// Users maps user names to passwords. Empty accepts any non-empty
	// credentials, which is what development stacks want.
	Users map[string]string
	// AccessTTL and RefreshTTL default to Oakestra's 10 minutes and 7 days.
	AccessTTL, RefreshTTL time.Duration
	// Now is the clock for token expiry and timestamps; time.Now when nil.
	Now func() time.Time
	// OnDeploy runs for every instance the fake places, after the instance
	// is recorded and before the deploy request returns. An error marks
	// the instance FAILED with the error as status_detail. cmd/ooa-fakeoak
	// uses it to start a fake engine on the requested host port.
	OnDeploy func(ctx context.Context, d Deployment) error
	// OnUndeploy runs for every instance that goes away: undeployed,
	// deleted with its service, or lost with its node.
	OnUndeploy func(d Deployment)
	// Logger defaults to a discarding logger.
	Logger *slog.Logger
}

// DefaultCSIDrivers are the drivers a node advertises when
// NodeSpec.CSIDrivers is nil.
var DefaultCSIDrivers = []string{oakestra.HostpathCSIDriver, oakestra.ModelpackCSIDriver}

// NodeSpec describes a simulated worker node. Zero fields get defaults that
// look like a DGX Spark reachable on localhost.
type NodeSpec struct {
	// Name is the hostname, used as candidate_name. Default "node<N>".
	Name string
	// IP defaults to 127.0.0.1 so host ports of fake engines work locally.
	// It may be a hostname, which is what a compose setup reports.
	IP string
	// Arch defaults to arm64.
	Arch string
	// MemoryMB is total RAM (default 131072); UsedMB is what the OS and
	// other software use (default 4096). The node reports available =
	// MemoryMB - UsedMB and the matching memory_percent, like the node
	// engine does.
	MemoryMB, UsedMB int
	// VCPUs defaults to 20.
	VCPUs int
	// GPUs defaults to 1; set NoGPU for a CPU-only node.
	GPUs  int
	NoGPU bool
	// VRAMMB is 0 by default, which is what unified-memory systems report.
	VRAMMB int
	// CSIDrivers are the CSI driver names the node advertises. nil means
	// DefaultCSIDrivers; an empty non-nil slice means none.
	CSIDrivers []string
}

// Deployment is an instance as the OnDeploy and OnUndeploy callbacks see it.
type Deployment struct {
	ServiceID      string
	ServiceName    string // microservice_name
	FullName       string // app.appns.service.servicens
	InstanceNumber int
	Node           oakestra.NodeResource
	ClusterName    string
	// HostPort and ContainerPort come from the first "host:container"
	// mapping of the SLA port string; both are 0 without one.
	HostPort, ContainerPort int
	Image                   string
	Cmd, Args               []string
	Environment             []string
	Volumes                 []oakestra.Volume
}

// Failure makes matching requests fail with Status.
type Failure struct {
	// Method matches the request method; empty matches any.
	Method string
	// Path is a prefix of the request path, e.g. "/api/service/".
	Path string
	// Status is the HTTP status to answer with.
	Status int
	// Message goes into the {"message": ...} body.
	Message string
	// Times limits how many requests fail; 0 means until ClearFailures.
	Times int
}

// ServiceView is a service with the SLA microservice it was created from,
// which keeps fields (args, volumes) the service document does not echo.
type ServiceView struct {
	oakestra.Service
	SLA oakestra.Microservice
}

type cluster struct {
	info oakestra.Cluster
}

type node struct {
	res       oakestra.NodeResource
	clusterID string
	totalMB   int
	usedMB    int
	active    bool
}

type app struct {
	info oakestra.ApplicationInfo
}

type service struct {
	view ServiceView
}

type token struct {
	user    string
	refresh bool
	exp     time.Time
}

// Fake is the in-memory Oakestra. Create it with New; it is safe for
// concurrent use.
type Fake struct {
	opts Options
	mux  *http.ServeMux
	log  *slog.Logger

	mu        sync.Mutex
	seq       int
	clusters  []*cluster
	nodes     map[string]*node
	nodeOrder []string
	apps      map[string]*app
	appOrder  []string
	services  map[string]*service
	svcOrder  []string
	tokens    map[string]token
	failures  []*Failure
	logins    int
	refreshes int
}

// New returns an empty fake: no clusters, no nodes.
func New(opts Options) *Fake {
	if opts.AccessTTL <= 0 {
		opts.AccessTTL = 10 * time.Minute
	}
	if opts.RefreshTTL <= 0 {
		opts.RefreshTTL = 7 * 24 * time.Hour
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	f := &Fake{
		opts:     opts,
		log:      opts.Logger,
		nodes:    make(map[string]*node),
		apps:     make(map[string]*app),
		services: make(map[string]*service),
		tokens:   make(map[string]token),
	}
	if f.log == nil {
		f.log = slog.New(slog.DiscardHandler)
	}
	f.routes()
	return f
}

// ServeHTTP implements http.Handler.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if fl := f.matchFailure(r); fl != nil {
		writeJSON(w, fl.Status, map[string]string{"message": fl.Message})
		return
	}
	f.mux.ServeHTTP(w, r)
}

// AbstractorPath is the path prefix under which the fake serves the
// resource abstractor of the named cluster. Append it to the fake's base
// URL to get the value for OOA_CLUSTER_ABSTRACTOR_OVERRIDES.
func AbstractorPath(clusterName string) string {
	return "/cluster/" + clusterName
}

// ClientOptions returns oakestra.Options that point a real client at a fake
// served on baseURL, with every cluster's abstractor resolved by name.
func ClientOptions(baseURL, user, password string) oakestra.Options {
	baseURL = strings.TrimRight(baseURL, "/")
	return oakestra.Options{
		API:      baseURL,
		User:     user,
		Password: password,
		AbstractorURL: func(name, _, _ string) string {
			return baseURL + AbstractorPath(name)
		},
	}
}

// AddCluster registers a cluster. ip is what /api/clusters/ reports.
func (f *Fake) AddCluster(name, ip string) oakestra.Cluster {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := &cluster{
		info: oakestra.Cluster{
			ID:            f.newIDLocked(),
			Name:          name,
			CandidateName: name,
			Location:      name + "-location",
			IP:            ip,
			Port:          "10100",
			Active:        true,
		},
	}
	f.clusters = append(f.clusters, c)
	return c.info
}

// Clusters returns the registered clusters.
func (f *Fake) Clusters() []oakestra.Cluster {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]oakestra.Cluster, 0, len(f.clusters))
	for _, c := range f.clusters {
		out = append(out, f.clusterViewLocked(c))
	}
	return out
}

// AddNode adds a node to the named cluster.
func (f *Fake) AddNode(clusterName string, spec NodeSpec) (oakestra.NodeResource, error) {
	f.mu.Lock()
	c := f.clusterByNameLocked(clusterName)
	if c == nil {
		f.mu.Unlock()
		return oakestra.NodeResource{}, fmt.Errorf("oakestratest: cluster %q not found", clusterName)
	}
	n := f.newNodeLocked(c, spec)
	for _, other := range f.nodes {
		if other.clusterID == c.info.ID && other.res.CandidateName == n.res.CandidateName {
			f.mu.Unlock()
			return oakestra.NodeResource{}, fmt.Errorf("oakestratest: node %q exists in cluster %q", n.res.CandidateName, clusterName)
		}
	}
	f.nodes[n.res.ID] = n
	f.nodeOrder = append(f.nodeOrder, n.res.ID)
	res := f.nodeViewLocked(n)
	f.mu.Unlock()
	return res, nil
}

func (f *Fake) newNodeLocked(c *cluster, s NodeSpec) *node {
	id := f.newIDLocked()
	for i := len(f.nodeOrder) + 1; s.Name == ""; i++ {
		name := fmt.Sprintf("node%d", i)
		if f.nodeLocked(name) == nil {
			s.Name = name
		}
	}
	if s.IP == "" {
		s.IP = "127.0.0.1"
	}
	if s.Arch == "" {
		s.Arch = "arm64"
	}
	if s.MemoryMB <= 0 {
		s.MemoryMB = 131072
	}
	if s.UsedMB <= 0 {
		s.UsedMB = 4096
	}
	if s.VCPUs <= 0 {
		s.VCPUs = 20
	}
	if s.GPUs <= 0 && !s.NoGPU {
		s.GPUs = 1
	}
	if s.NoGPU {
		s.GPUs = 0
		s.VRAMMB = 0
	}
	res := oakestra.NodeResource{
		ID:             id,
		CandidateName:  s.Name,
		Host:           s.Name,
		IP:             s.IP,
		Port:           "50103",
		Architecture:   s.Arch,
		VCPUs:          s.VCPUs,
		CPUPercent:     2.5,
		VRAMMB:         float64(s.VRAMMB),
		VGPUs:          s.GPUs,
		Virtualization: []string{"docker"},
		SystemInfo:     map[string]string{"kernel_version": "6.11.0-fake", "os": "linux"},
	}
	if s.GPUs > 0 {
		res.GPUDriver = "580.95"
		res.GPUTemp = 41
	}
	drivers := s.CSIDrivers
	if drivers == nil {
		drivers = DefaultCSIDrivers
	}
	res.CSIDrivers = csiDrivers(drivers)
	return &node{res: res, clusterID: c.info.ID, totalMB: s.MemoryMB, usedMB: min(s.UsedMB, s.MemoryMB), active: true}
}

// NodeUpdate changes a node; nil fields stay as they are.
type NodeUpdate struct {
	UsedMB     *int
	MemoryMB   *int
	Active     *bool
	IP         *string
	VRAMMB     *int
	CPUPercent *float64
	// CSIDrivers replaces the advertised driver names; an empty slice
	// removes them all.
	CSIDrivers *[]string
}

// UpdateNode applies u to the node with the given id or name.
func (f *Fake) UpdateNode(idOrName string, u NodeUpdate) (oakestra.NodeResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.nodeLocked(idOrName)
	if n == nil {
		return oakestra.NodeResource{}, fmt.Errorf("oakestratest: node %q not found", idOrName)
	}
	if u.MemoryMB != nil {
		n.totalMB = *u.MemoryMB
	}
	if u.UsedMB != nil {
		n.usedMB = *u.UsedMB
	}
	n.usedMB = min(n.usedMB, n.totalMB)
	if u.Active != nil {
		n.active = *u.Active
	}
	if u.IP != nil {
		n.res.IP = *u.IP
	}
	if u.VRAMMB != nil {
		n.res.VRAMMB = float64(*u.VRAMMB)
	}
	if u.CPUPercent != nil {
		n.res.CPUPercent = *u.CPUPercent
	}
	if u.CSIDrivers != nil {
		n.res.CSIDrivers = csiDrivers(*u.CSIDrivers)
	}
	return f.nodeViewLocked(n), nil
}

// RemoveNode deletes the node with the given id or name. Instances on it
// are marked FAILED and OnUndeploy runs for each, the way a lost worker
// ends its containers.
func (f *Fake) RemoveNode(idOrName string) error {
	f.mu.Lock()
	n := f.nodeLocked(idOrName)
	if n == nil {
		f.mu.Unlock()
		return fmt.Errorf("oakestratest: node %q not found", idOrName)
	}
	delete(f.nodes, n.res.ID)
	f.nodeOrder = slices.DeleteFunc(f.nodeOrder, func(id string) bool { return id == n.res.ID })
	var lost []Deployment
	for _, id := range f.svcOrder {
		s := f.services[id]
		for i := range s.view.InstanceList {
			inst := &s.view.InstanceList[i]
			if inst.WorkerID == n.res.ID && inst.Status != oakestra.StatusFailed {
				inst.Status = oakestra.StatusFailed
				inst.StatusDetail = "worker node removed"
				lost = append(lost, f.deploymentLocked(s, *inst, n))
			}
		}
	}
	f.mu.Unlock()
	if f.opts.OnUndeploy != nil {
		for _, d := range lost {
			f.opts.OnUndeploy(d)
		}
	}
	return nil
}

// Nodes returns every node as the abstractor reports it, in insertion
// order.
func (f *Fake) Nodes() []oakestra.NodeResource {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]oakestra.NodeResource, 0, len(f.nodeOrder))
	for _, id := range f.nodeOrder {
		out = append(out, f.nodeViewLocked(f.nodes[id]))
	}
	return out
}

// Node returns one node by id or name.
func (f *Fake) Node(idOrName string) (oakestra.NodeResource, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.nodeLocked(idOrName)
	if n == nil {
		return oakestra.NodeResource{}, false
	}
	return f.nodeViewLocked(n), true
}

// Applications returns every application.
func (f *Fake) Applications() []oakestra.ApplicationInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]oakestra.ApplicationInfo, 0, len(f.appOrder))
	for _, id := range f.appOrder {
		out = append(out, cloneApp(f.apps[id].info))
	}
	return out
}

// Services returns every service, in creation order.
func (f *Fake) Services() []ServiceView {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ServiceView, 0, len(f.svcOrder))
	for _, id := range f.svcOrder {
		out = append(out, cloneView(f.services[id].view))
	}
	return out
}

// Service returns one service by id.
func (f *Fake) Service(id string) (ServiceView, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.services[id]
	if !ok {
		return ServiceView{}, false
	}
	return cloneView(s.view), true
}

// Fail adds a failure rule. Rules are checked in the order added.
func (f *Fake) Fail(fl Failure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, &fl)
}

// ClearFailures removes every failure rule.
func (f *Fake) ClearFailures() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = nil
}

// ExpireTokens invalidates every access token, as a restart of the system
// manager with a new secret would. Refresh tokens stay valid.
func (f *Fake) ExpireTokens() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, t := range f.tokens {
		if !t.refresh {
			delete(f.tokens, k)
		}
	}
}

// Logins and Refreshes count successful logins and token refreshes.
func (f *Fake) Logins() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins
}

// Refreshes counts successful token refreshes.
func (f *Fake) Refreshes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshes
}

func (f *Fake) matchFailure(r *http.Request) *Failure {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, fl := range f.failures {
		if fl.Method != "" && fl.Method != r.Method {
			continue
		}
		if !strings.HasPrefix(r.URL.Path, fl.Path) {
			continue
		}
		out := *fl
		if fl.Times > 0 {
			fl.Times--
			if fl.Times == 0 {
				f.failures = slices.Delete(f.failures, i, i+1)
			}
		}
		return &out
	}
	return nil
}

func (f *Fake) newIDLocked() string {
	f.seq++
	return fmt.Sprintf("%024x", f.seq)
}

func (f *Fake) clusterByNameLocked(name string) *cluster {
	for _, c := range f.clusters {
		if c.info.Name == name {
			return c
		}
	}
	return nil
}

func (f *Fake) clusterByIDLocked(id string) *cluster {
	for _, c := range f.clusters {
		if c.info.ID == id {
			return c
		}
	}
	return nil
}

func (f *Fake) nodeLocked(idOrName string) *node {
	if n, ok := f.nodes[idOrName]; ok {
		return n
	}
	for _, id := range f.nodeOrder {
		if f.nodes[id].res.CandidateName == idOrName {
			return f.nodes[id]
		}
	}
	return nil
}

// nodeViewLocked renders a node the way the node engine reports it:
// memory is available MiB and memory_percent is used percent.
func (f *Fake) nodeViewLocked(n *node) oakestra.NodeResource {
	res := n.res
	res.Virtualization = slices.Clone(n.res.Virtualization)
	res.CSIDrivers = slices.Clone(n.res.CSIDrivers)
	res.SystemInfo = maps.Clone(n.res.SystemInfo)
	res.MemoryMB = n.totalMB - n.usedMB
	if n.totalMB > 0 {
		res.MemoryPercent = math.Round(float64(n.usedMB)/float64(n.totalMB)*1000) / 10
	}
	res.Active = n.active
	now := f.opts.Now()
	if !n.active {
		// The abstractor's freshness window is 30 s.
		now = now.Add(-time.Minute)
	}
	res.LastModified = oakestra.Timestamp{Time: now.UTC()}
	return res
}

func (f *Fake) clusterViewLocked(c *cluster) oakestra.Cluster {
	info := c.info
	info.ActiveNodes = 0
	info.MemoryMB = 0
	info.VCPUs, info.VGPUs = 0, 0
	csi := map[string]bool{}
	for _, id := range f.nodeOrder {
		n := f.nodes[id]
		if n.clusterID != c.info.ID || !n.active {
			continue
		}
		info.ActiveNodes++
		info.MemoryMB += float64(n.totalMB - n.usedMB)
		info.VCPUs += float64(n.res.VCPUs)
		info.VGPUs += float64(n.res.VGPUs)
		for _, d := range n.res.CSIDrivers {
			csi[d.Name] = true
		}
	}
	info.CSIDrivers = nil
	for _, name := range slices.Sorted(maps.Keys(csi)) {
		info.CSIDrivers = append(info.CSIDrivers, oakestra.CSIDriver{Name: name})
	}
	info.Virtualization = []string{"docker"}
	info.LastModified = oakestra.Timestamp{Time: f.opts.Now().UTC()}
	return info
}

func (f *Fake) deploymentLocked(s *service, inst oakestra.Instance, n *node) Deployment {
	d := Deployment{
		ServiceID:      s.view.ID,
		ServiceName:    s.view.MicroserviceName,
		FullName:       oakestra.FullServiceName(s.view.AppName, s.view.AppNS, s.view.ServiceName, s.view.ServiceNS),
		InstanceNumber: inst.InstanceNumber,
		Image:          s.view.Image,
		Cmd:            slices.Clone(s.view.Cmd),
		Args:           slices.Clone(s.view.SLA.Args),
		Environment:    slices.Clone(s.view.Environment),
		Volumes:        slices.Clone(s.view.SLA.Volumes),
	}
	d.HostPort, d.ContainerPort = firstPortMapping(s.view.Port)
	if n != nil {
		d.Node = f.nodeViewLocked(n)
		if c := f.clusterByIDLocked(n.clusterID); c != nil {
			d.ClusterName = c.info.Name
		}
	}
	return d
}

// firstPortMapping parses the first "host:container[/proto]" entry of an
// SLA port string. A bare "port" publishes the same port on both sides.
func firstPortMapping(port string) (host, ctr int) {
	first, _, _ := strings.Cut(port, ";")
	first, _, _ = strings.Cut(first, "/")
	if first == "" {
		return 0, 0
	}
	h, c, ok := strings.Cut(first, ":")
	if !ok {
		c = h
	}
	hp, err1 := strconv.Atoi(strings.TrimSpace(h))
	cp, err2 := strconv.Atoi(strings.TrimSpace(c))
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return hp, cp
}

func csiDrivers(names []string) []oakestra.CSIDriver {
	out := make([]oakestra.CSIDriver, 0, len(names))
	for _, name := range names {
		out = append(out, oakestra.CSIDriver{Name: name, Endpoint: "/run/csi/" + strings.NewReplacer("/", "-", ".", "-").Replace(name) + ".sock"})
	}
	return out
}

func cloneApp(a oakestra.ApplicationInfo) oakestra.ApplicationInfo {
	a.Microservices = slices.Clone(a.Microservices)
	if a.Microservices == nil {
		a.Microservices = []string{}
	}
	return a
}

func cloneView(v ServiceView) ServiceView {
	v.InstanceList = slices.Clone(v.InstanceList)
	if v.InstanceList == nil {
		v.InstanceList = []oakestra.Instance{}
	}
	v.Cmd = slices.Clone(v.Cmd)
	v.Environment = slices.Clone(v.Environment)
	v.Constraints = slices.Clone(v.Constraints)
	v.SLA.Volumes = slices.Clone(v.SLA.Volumes)
	return v
}

// newTokenLocked issues a JWT-shaped token (unsigned) so the client's exp
// parsing runs against the same shape as with the real system manager.
func (f *Fake) newTokenLocked(user string, refresh bool) string {
	ttl := f.opts.AccessTTL
	kind := "access"
	if refresh {
		ttl = f.opts.RefreshTTL
		kind = "refresh"
	}
	exp := f.opts.Now().Add(ttl)
	f.seq++
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"sub": user, "exp": exp.Unix(), "type": kind, "jti": strconv.Itoa(f.seq)})
	tok := header + "." + base64.RawURLEncoding.EncodeToString(claims) + ".fake"
	f.tokens[tok] = token{user: user, refresh: refresh, exp: exp}
	return tok
}

var errNoToken = errors.New("missing authentication token")

// authLocked returns the user of a valid bearer token of the given kind.
func (f *Fake) authLocked(r *http.Request, refresh bool) (string, error) {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || tok == "" {
		return "", errNoToken
	}
	t, ok := f.tokens[tok]
	if !ok || t.refresh != refresh {
		return "", errNoToken
	}
	if !f.opts.Now().Before(t.exp) {
		return "", errors.New("token has expired")
	}
	return t.user, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}
