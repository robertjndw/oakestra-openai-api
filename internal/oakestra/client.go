package oakestra

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Client is everything the addon does against Oakestra. Methods are safe
// for concurrent use. Authentication (login, refresh on 401) is handled
// inside the implementation; callers never see tokens.
type Client interface {
	// ListClusters returns every registered cluster (GET /api/clusters/).
	ListClusters(ctx context.Context) ([]Cluster, error)
	// ListNodes returns the worker nodes of one cluster from its resource
	// abstractor (Options.AbstractorURL picks the base URL).
	ListNodes(ctx context.Context, cluster Cluster) ([]NodeResource, error)

	// CreateApplication creates an application with no services and
	// returns its info. ErrConflict if name and namespace exist already.
	CreateApplication(ctx context.Context, app Application) (ApplicationInfo, error)
	// GetApplications lists the service account's applications.
	GetApplications(ctx context.Context) ([]ApplicationInfo, error)

	// CreateService adds one microservice to an application and returns
	// the new service id.
	CreateService(ctx context.Context, appID string, ms Microservice) (string, error)
	// ListServices returns the services of one application
	// (GET /api/services/{appID}), for adopting a service by name after a
	// crash between creating it and recording its id.
	ListServices(ctx context.Context, appID string) ([]Service, error)
	// GetService returns a service with its instance list. ErrNotFound if
	// it does not exist.
	GetService(ctx context.Context, serviceID string) (Service, error)
	// DeleteService undeploys every instance and removes the service.
	// Deleting a missing service returns ErrNotFound.
	DeleteService(ctx context.Context, serviceID string) error

	// DeployInstance asks the scheduler for one more instance
	// (POST /api/service/{id}/instance). Scheduling happens in the
	// background; poll GetService for the outcome.
	DeployInstance(ctx context.Context, serviceID string) error
	// UndeployInstance removes one instance
	// (DELETE /api/service/{id}/instance/{n}).
	UndeployInstance(ctx context.Context, serviceID string, instance int) error
}

// EnsureApplication returns the application named app.ApplicationName in
// app.ApplicationNamespace, creating it when it does not exist. Create is
// tried first because it is one request in the common case and the
// conflict answer is cheap.
func EnsureApplication(ctx context.Context, c Client, app Application) (ApplicationInfo, error) {
	info, err := c.CreateApplication(ctx, app)
	if err == nil {
		return info, nil
	}
	if !errors.Is(err, ErrConflict) {
		return ApplicationInfo{}, err
	}
	apps, err := c.GetApplications(ctx)
	if err != nil {
		return ApplicationInfo{}, err
	}
	for _, a := range apps {
		if a.ApplicationName == app.ApplicationName && a.ApplicationNamespace == app.ApplicationNamespace {
			return a, nil
		}
	}
	// A conflict for an application the account cannot see means another
	// user owns that name and namespace.
	return ApplicationInfo{}, &APIError{Op: "ensure application", Status: http.StatusConflict,
		Message: fmt.Sprintf("%s.%s exists but is not owned by this account", app.ApplicationName, app.ApplicationNamespace)}
}

// Sentinel errors; match with errors.Is. Every error from Client that
// carries an HTTP status is an *APIError that matches the sentinel for its
// status.
var (
	ErrUnauthorized = errors.New("oakestra: unauthorized")
	ErrForbidden    = errors.New("oakestra: forbidden")
	ErrNotFound     = errors.New("oakestra: not found")
	ErrConflict     = errors.New("oakestra: conflict")
	ErrInvalid      = errors.New("oakestra: invalid request")
	ErrUnavailable  = errors.New("oakestra: unavailable")
)

// APIError is a non-2xx response from an Oakestra component.
type APIError struct {
	// Op names the call, e.g. "create service".
	Op string
	// Status is the HTTP status code.
	Status int
	// Message is the server's message, extracted from {"message": ...}
	// when present, otherwise a body excerpt.
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("oakestra: %s: %d %s: %s", e.Op, e.Status, http.StatusText(e.Status), e.Message)
}

// Is maps the status to the sentinel errors.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.Status == http.StatusUnauthorized
	case ErrForbidden:
		return e.Status == http.StatusForbidden
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrConflict:
		return e.Status == http.StatusConflict
	case ErrInvalid:
		return e.Status == http.StatusBadRequest || e.Status == http.StatusUnprocessableEntity
	case ErrUnavailable:
		return e.Status == http.StatusBadGateway || e.Status == http.StatusServiceUnavailable || e.Status == http.StatusGatewayTimeout
	default:
		return false
	}
}

// LoginRequest is the body of POST /api/auth/login.
type LoginRequest struct {
	Username         string `json:"username"`
	Password         string `json:"password"`
	OrganizationName string `json:"organization_name,omitempty"`
}

// LoginResponse is returned by login; refresh returns only Token.
type LoginResponse struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// FullServiceName is the name the service manager indexes services by.
func FullServiceName(app, appNS, service, serviceNS string) string {
	return app + "." + appNS + "." + service + "." + serviceNS
}
