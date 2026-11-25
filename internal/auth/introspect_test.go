package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertjndw/oakestra-openai-api/internal/config"
)

func jwt(sub string) string {
	p, _ := json.Marshal(map[string]any{"sub": sub, "roles": []string{"Admin"}})
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(p) + ".sig"
}

// systemManager mimics GET /api/user/<name>.
func systemManager(t *testing.T, status int, body string, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !strings.HasPrefix(r.URL.Path, "/api/user/") || r.Header.Get("Authorization") == "" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestIntrospectShapes(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		roles []string
		admin bool
	}{
		{"real string roles", `{"_id":"1","name":"alice","email":"a@b","roles":["Admin","Application_Provider"]}`, []string{"Admin", "Application_Provider"}, true},
		{"mock lowercase", `{"name":"alice","roles":["admin"]}`, []string{"admin"}, true},
		{"object roles", `{"name":"alice","roles":[{"name":"Admin"},{"role":"Infrastructure_Provider"}]}`, []string{"Admin", "Infrastructure_Provider"}, true},
		{"org admin is not admin", `{"name":"alice","roles":["Organization_Admin"]}`, []string{"Organization_Admin"}, false},
		{"no roles key", `{"name":"alice"}`, []string{}, false},
		{"null roles", `{"name":"alice","roles":null}`, []string{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := systemManager(t, 200, tt.body, &calls)
			u, err := NewIntrospector(srv.URL+"/api/", srv.Client()).Introspect(t.Context(), jwt("alice"))
			if err != nil {
				t.Fatal(err)
			}
			if u.Name != "alice" || u.Admin != tt.admin || strings.Join(u.Roles, ",") != strings.Join(tt.roles, ",") || u.Roles == nil {
				t.Fatalf("user = %+v", u)
			}
		})
	}
}

func TestIntrospectRejections(t *testing.T) {
	tests := []struct {
		name   string
		token  string
		status int
		body   string
		want   error
	}{
		{"sub mismatch", jwt("alice"), 200, `{"name":"mallory","roles":["Admin"]}`, ErrInvalidToken},
		{"null body for unknown user", jwt("alice"), 200, `null`, ErrInvalidToken},
		{"expired", jwt("alice"), 401, `{"msg":"Token has expired"}`, ErrInvalidToken},
		{"malformed per flask-jwt", jwt("alice"), 422, `{}`, ErrInvalidToken},
		{"wrong identity", jwt("alice"), 403, `{}`, ErrInvalidToken},
		{"not a jwt", "abc", 200, `{}`, ErrInvalidToken},
		{"no sub", jwt(""), 200, `{}`, ErrInvalidToken},
		{"manager down", jwt("alice"), 502, `{}`, ErrIntrospectUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := systemManager(t, tt.status, tt.body, &calls)
			_, err := NewIntrospector(srv.URL+"/api", srv.Client()).Introspect(t.Context(), tt.token)
			if !isErr(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func isErr(err, target error) bool {
	return err != nil && strings.Contains(err.Error(), target.Error())
}

func TestIntrospectUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := NewIntrospector(url+"/api", nil).Introspect(t.Context(), jwt("alice"))
	if !isErr(err, ErrIntrospectUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestIntrospectCaches(t *testing.T) {
	var calls atomic.Int32
	srv := systemManager(t, 200, `{"name":"alice","roles":["Admin"]}`, &calls)
	in := NewIntrospector(srv.URL+"/api", srv.Client())
	now := time.Now()
	in.now = func() time.Time { return now }
	tok := jwt("alice")
	for range 3 {
		if _, err := in.Introspect(t.Context(), tok); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
	now = now.Add(61 * time.Second)
	if _, err := in.Introspect(t.Context(), tok); err != nil || calls.Load() != 2 {
		t.Fatalf("after ttl: calls = %d, err = %v", calls.Load(), err)
	}
	for k := range in.cache {
		if strings.Contains(string(k[:]), "alice") {
			t.Fatal("token material in cache key")
		}
	}
}

func serve(t *testing.T, mode config.AdminAuthMode, in *Introspector, admin bool, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := UserFrom(r.Context())
		if !ok {
			t.Error("no user in context")
		}
		_ = json.NewEncoder(w).Encode(u)
	})
	if admin {
		h = RequireAdmin(h)
	}
	rec := httptest.NewRecorder()
	Require(mode, in)(h).ServeHTTP(rec, req)
	return rec
}

func TestMiddleware(t *testing.T) {
	var calls atomic.Int32
	adminSrv := systemManager(t, 200, `{"name":"alice","roles":["Admin"]}`, &calls)
	userSrv := systemManager(t, 200, `{"name":"bob","roles":["Application_Provider"]}`, &calls)
	downSrv := systemManager(t, 500, `{}`, &calls)

	get := func(tok string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/admin/v1/me", http.NoBody)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		return r
	}
	intro := func(s *httptest.Server) *Introspector { return NewIntrospector(s.URL+"/api", s.Client()) }

	if rec := serve(t, config.AdminAuthIntrospect, intro(adminSrv), true, get(jwt("alice"))); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"admin":true`) {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(t, config.AdminAuthIntrospect, intro(userSrv), false, get(jwt("bob"))); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"roles":["Application_Provider"]`) {
		t.Fatalf("user: %d %s", rec.Code, rec.Body)
	}
	for _, tt := range []struct {
		name  string
		in    *Introspector
		req   *http.Request
		admin bool
		want  int
	}{
		{"no token", intro(adminSrv), get(""), false, 401},
		{"bad token", intro(adminSrv), get("garbage"), false, 401},
		{"mismatch", intro(adminSrv), get(jwt("eve")), false, 401},
		{"non admin", intro(userSrv), get(jwt("bob")), true, 403},
		{"upstream failure", intro(downSrv), get(jwt("alice")), false, 503},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var h http.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
			if tt.admin {
				h = RequireAdmin(h)
			}
			rec := httptest.NewRecorder()
			Require(config.AdminAuthIntrospect, tt.in)(h).ServeHTTP(rec, tt.req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.want, rec.Body)
			}
		})
	}

	rec := serve(t, config.AdminAuthNone, nil, true, get(""))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"admin":true`) {
		t.Fatalf("none mode: %d %s", rec.Code, rec.Body)
	}
}

func TestRequireAdminWithoutUser(t *testing.T) {
	rec := httptest.NewRecorder()
	RequireAdmin(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if rec.Code != 401 {
		t.Fatalf("status = %d", rec.Code)
	}
}
