package oakestra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/config"
)

// DefaultTimeout bounds one HTTP request to Oakestra when Options.Timeout is
// zero. The system manager answers synchronously except for scheduling, which
// it does in a background thread, so nothing we call should take long.
const DefaultTimeout = 30 * time.Second

// maxResponseBytes caps how much of a response body we read. Service and
// node lists of a large cluster stay far below it.
const maxResponseBytes = 32 << 20

// Options configures an HTTPClient.
type Options struct {
	// API is the system-manager base URL (OOA_OAKESTRA_API).
	API string
	// User, Password and Org are the service-account credentials. Org is the
	// organization name; empty logs in to the root organization.
	User, Password, Org string
	// AbstractorURL returns the base URL of a cluster's resource abstractor,
	// without trailing slash. Defaults to http://<ip>:11012.
	AbstractorURL func(clusterName, clusterID, clusterIP string) string
	// HTTPClient defaults to a client with sane dial and idle timeouts.
	HTTPClient *http.Client
	// Timeout bounds each request; DefaultTimeout when zero.
	Timeout time.Duration
	// Now is the clock used for token expiry; time.Now when nil.
	Now func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// OptionsFromConfig maps the server configuration to client options.
func OptionsFromConfig(cfg config.Config) Options {
	return Options{
		API:           cfg.Oakestra.API,
		User:          cfg.Oakestra.User,
		Password:      cfg.Oakestra.Password,
		Org:           cfg.Oakestra.Org,
		AbstractorURL: cfg.AbstractorURL,
	}
}

// HTTPClient is the Client implementation against a real Oakestra.
type HTTPClient struct {
	opts Options
	hc   *http.Client
	log  *slog.Logger
	now  func() time.Time

	authMu     sync.Mutex
	access     string
	accessExp  time.Time
	refresh    string
	refreshExp time.Time
	inflight   *authCall

	cacheMu sync.Mutex
	userID  string
	apps    map[string]ApplicationInfo
}

var _ Client = (*HTTPClient)(nil)

// New validates opts and returns a client. It does not contact Oakestra;
// the first call logs in.
func New(opts Options) (*HTTPClient, error) {
	if opts.API == "" {
		return nil, errors.New("oakestra: API URL is empty")
	}
	if opts.User == "" {
		return nil, errors.New("oakestra: user is empty")
	}
	opts.API = strings.TrimRight(opts.API, "/")
	if opts.AbstractorURL == nil {
		opts.AbstractorURL = func(_, _, ip string) string {
			return "http://" + net.JoinHostPort(ip, "11012")
		}
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	c := &HTTPClient{
		opts: opts,
		hc:   opts.HTTPClient,
		log:  opts.Logger,
		now:  opts.Now,
		apps: make(map[string]ApplicationInfo),
	}
	if c.hc == nil {
		c.hc = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: opts.Timeout,
		}}
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// AbstractorBaseURL returns the resource-abstractor base URL used for cluster.
func (c *HTTPClient) AbstractorBaseURL(cluster Cluster) string {
	return strings.TrimRight(c.opts.AbstractorURL(cluster.DisplayName(), cluster.ID, cluster.IP), "/")
}

// ListClusters implements Client. The endpoint is public, so node listing
// keeps working while the service-account credentials are wrong.
func (c *HTTPClient) ListClusters(ctx context.Context) ([]Cluster, error) {
	var out []Cluster
	if err := c.send(ctx, "list clusters", http.MethodGet, c.opts.API+"/api/clusters/", "", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListNodes implements Client.
func (c *HTTPClient) ListNodes(ctx context.Context, cluster Cluster) ([]NodeResource, error) {
	u := c.AbstractorBaseURL(cluster) + "/api/v1/resources/?" + url.Values{"resources": {NodeResourceFields}}.Encode()
	var out []NodeResource
	if err := c.send(ctx, "list nodes of "+cluster.DisplayName(), http.MethodGet, u, "", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateApplication implements Client.
func (c *HTTPClient) CreateApplication(ctx context.Context, app Application) (ApplicationInfo, error) {
	app.ApplicationID = ""
	// ApplicationSLA turns nil microservices into []: register_app calls
	// len() on them and null would be a 500.
	sla := ApplicationSLA(c.opts.User, app)
	var apps []ApplicationInfo
	if err := c.call(ctx, "create application", http.MethodPost, c.opts.API+"/api/application/", sla, &apps); err != nil {
		return ApplicationInfo{}, err
	}
	c.cacheApps(apps)
	for _, a := range apps {
		if a.ApplicationName == app.ApplicationName && a.ApplicationNamespace == app.ApplicationNamespace {
			return a, nil
		}
	}
	return ApplicationInfo{}, fmt.Errorf("oakestra: create application: %s.%s missing from the response", app.ApplicationName, app.ApplicationNamespace)
}

// GetApplications implements Client. Listing all applications needs the
// ADMIN role, so this goes through the per-user endpoint, which wants the
// user's document id rather than the name.
func (c *HTTPClient) GetApplications(ctx context.Context) ([]ApplicationInfo, error) {
	uid, err := c.ownUserID(ctx)
	if err != nil {
		return nil, err
	}
	var apps []ApplicationInfo
	if err := c.call(ctx, "list applications", http.MethodGet, c.opts.API+"/api/applications/"+url.PathEscape(uid), nil, &apps); err != nil {
		return nil, err
	}
	c.cacheApps(apps)
	return apps, nil
}

func (c *HTTPClient) ownUserID(ctx context.Context) (string, error) {
	c.cacheMu.Lock()
	uid := c.userID
	c.cacheMu.Unlock()
	if uid != "" {
		return uid, nil
	}
	var user struct {
		ID mongoID `json:"_id"`
	}
	if err := c.call(ctx, "get user", http.MethodGet, c.opts.API+"/api/user/"+url.PathEscape(c.opts.User), nil, &user); err != nil {
		return "", err
	}
	if user.ID == "" {
		return "", fmt.Errorf("oakestra: get user %s: no _id in response", c.opts.User)
	}
	c.cacheMu.Lock()
	c.userID = string(user.ID)
	c.cacheMu.Unlock()
	return string(user.ID), nil
}

func (c *HTTPClient) cacheApps(apps []ApplicationInfo) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	for _, a := range apps {
		if a.ApplicationID != "" {
			c.apps[a.ApplicationID] = a
		}
	}
}

func (c *HTTPClient) application(ctx context.Context, appID string) (ApplicationInfo, error) {
	c.cacheMu.Lock()
	a, ok := c.apps[appID]
	c.cacheMu.Unlock()
	if ok {
		return a, nil
	}
	apps, err := c.GetApplications(ctx)
	if err != nil {
		return ApplicationInfo{}, err
	}
	for _, a := range apps {
		if a.ApplicationID == appID {
			return a, nil
		}
	}
	return ApplicationInfo{}, &APIError{Op: "create service", Status: http.StatusNotFound, Message: "application " + appID + " not found"}
}

// CreateService implements Client. The SLA schema requires the
// application's name and namespace next to its id, so they are looked up
// (and cached) first.
func (c *HTTPClient) CreateService(ctx context.Context, appID string, ms Microservice) (string, error) {
	app, err := c.application(ctx, appID)
	if err != nil {
		return "", err
	}
	sla := ServiceSLA(c.opts.User, app, ms)
	var res CreateServiceResult
	if err := c.call(ctx, "create service", http.MethodPost, c.opts.API+"/api/service/", sla, &res); err != nil {
		return "", err
	}
	// The endpoint answers 200 even when a service failed; the per-service
	// status says what happened.
	if len(res.FailedServices) > 0 {
		f := res.FailedServices[0]
		status := f.Status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		return "", &APIError{Op: "create service " + f.ServiceName, Status: status, Message: f.Message}
	}
	if res.JobID == "" {
		return "", fmt.Errorf("oakestra: create service %s: no job id in response", ms.MicroserviceName)
	}
	return res.JobID, nil
}

// GetService implements Client.
func (c *HTTPClient) GetService(ctx context.Context, serviceID string) (Service, error) {
	var s Service
	if err := c.call(ctx, "get service", http.MethodGet, c.serviceURL(serviceID), nil, &s); err != nil {
		return Service{}, err
	}
	if s.ID == "" {
		return Service{}, &APIError{Op: "get service", Status: http.StatusNotFound, Message: "service " + serviceID + " not found"}
	}
	return s, nil
}

// ListServices implements Client.
func (c *HTTPClient) ListServices(ctx context.Context, appID string) ([]Service, error) {
	var out []Service
	if err := c.call(ctx, "list services", http.MethodGet, c.opts.API+"/api/services/"+url.PathEscape(appID), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteService implements Client. The system manager answers 500 "Job
// not deleted" for a service that does not exist, so a 500 is checked with
// a GET before it is reported.
func (c *HTTPClient) DeleteService(ctx context.Context, serviceID string) error {
	err := c.call(ctx, "delete service", http.MethodDelete, c.serviceURL(serviceID), nil, nil)
	if apiErr, ok := errors.AsType[*APIError](err); ok && apiErr.Status == http.StatusInternalServerError {
		if _, getErr := c.GetService(ctx, serviceID); errors.Is(getErr, ErrNotFound) {
			return getErr
		}
	}
	return err
}

// DeployInstance implements Client.
func (c *HTTPClient) DeployInstance(ctx context.Context, serviceID string) error {
	return c.call(ctx, "deploy instance", http.MethodPost, c.serviceURL(serviceID)+"/instance", nil, nil)
}

// UndeployInstance implements Client.
func (c *HTTPClient) UndeployInstance(ctx context.Context, serviceID string, instance int) error {
	u := c.serviceURL(serviceID) + "/instance/" + strconv.Itoa(instance)
	return c.call(ctx, "undeploy instance", http.MethodDelete, u, nil, nil)
}

func (c *HTTPClient) serviceURL(serviceID string) string {
	return c.opts.API + "/api/service/" + url.PathEscape(serviceID)
}

// call sends an authenticated request. A 401 is retried once with a fresh
// token, because the server may have rejected a token our clock still
// considered valid (clock skew, server restart with a new secret).
func (c *HTTPClient) call(ctx context.Context, op, method, u string, body, out any) error {
	tok, err := c.token(ctx, "")
	if err != nil {
		return fmt.Errorf("oakestra: %s: authenticate: %w", op, err)
	}
	err = c.send(ctx, op, method, u, tok, body, out)
	if !errors.Is(err, ErrUnauthorized) {
		return err
	}
	tok, err = c.token(ctx, tok)
	if err != nil {
		return fmt.Errorf("oakestra: %s: authenticate: %w", op, err)
	}
	return c.send(ctx, op, method, u, tok, body, out)
}

// send performs one request. bearer may be empty for unauthenticated
// endpoints.
func (c *HTTPClient) send(ctx context.Context, op, method, u, bearer string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("oakestra: %s: encode request: %w", op, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return fmt.Errorf("oakestra: %s: %w", op, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		// Network failures and timeouts are reported as unavailable so
		// callers can treat "Oakestra is down" uniformly.
		return fmt.Errorf("oakestra: %s: %w: %w", op, ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("oakestra: %s: read response: %w: %w", op, ErrUnavailable, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Op: op, Status: resp.StatusCode, Message: errorMessage(raw)}
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("oakestra: %s: decode response: %w", op, err)
	}
	return nil
}

// errorMessage extracts a readable message from the different error shapes
// Oakestra components produce: {"message": ...} (flask-smorest and our own
// aborts), {"msg": ...} (flask-jwt-extended), or a plain string.
func errorMessage(raw []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"message", "msg", "description", "error"} {
			v, ok := m[k]
			if !ok {
				continue
			}
			var s string
			if json.Unmarshal(v, &s) == nil {
				return s
			}
			return excerpt(v)
		}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return excerpt(raw)
}

func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	const limit = 300
	if len(s) > limit {
		s = s[:limit] + "..."
	}
	return s
}

// mongoID decodes a Mongo document id that the server serialized either as
// a string or in extended JSON ({"$oid": "..."}).
type mongoID string

func (m *mongoID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*m = mongoID(s)
		return nil
	}
	var o struct {
		OID string `json:"$oid"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return fmt.Errorf("oakestra: id: %w", err)
	}
	*m = mongoID(o.OID)
	return nil
}
