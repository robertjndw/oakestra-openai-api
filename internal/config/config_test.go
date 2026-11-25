package config

import (
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"
)

func base() map[string]string {
	return map[string]string{
		"OOA_OAKESTRA_USER":     "ooa",
		"OOA_OAKESTRA_PASSWORD": "secret",
	}
}

func with(m map[string]string, kv ...string) map[string]string {
	out := maps.Clone(m)
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func TestLoadDefaults(t *testing.T) {
	c, err := LoadFrom(base())
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"Listen", c.Listen, ":8080"},
		{"AdminListen", c.AdminListen, ":8081"},
		{"PublicURL", c.PublicURL, ""},
		{"DataDir", c.DataDir, "/data"},
		{"LogLevel", c.LogLevel, slog.LevelInfo},
		{"Oakestra.API", c.Oakestra.API, "http://system_manager:10000"},
		{"Oakestra.Org", c.Oakestra.Org, ""},
		{"ClusterAbstractorPort", c.ClusterAbstractorPort, 11012},
		{"AppName", c.AppName, "openaiapi"},
		{"AppNS", c.AppNS, "ooa"},
		{"EnginePortRange", c.EnginePortRange, PortRange{Lo: 18000, Hi: 18999}},
		{"DefaultEngineImage", c.DefaultEngineImage, "vllm/vllm-openai:v0.29.0"},
		{"ModelCSIDriver", c.ModelCSIDriver, "model.csi.modelpack.org"},
		{"ModelCSIRefKey", c.ModelCSIRefKey, "model.csi.modelpack.org/reference"},
		{"ModelMountPath", c.ModelMountPath, "/model"},
		{"PollInterval", c.PollInterval, 5 * time.Second},
		{"ScheduleTimeout", c.ScheduleTimeout, 15 * time.Minute},
		{"StartupTimeout", c.StartupTimeout, 30 * time.Minute},
		{"Scheduler", c.Scheduler, "least-inflight"},
		{"MaxAttempts", c.MaxAttempts, 3},
		{"MaxBodyBytes", c.MaxBodyBytes, ByteSize(16 << 20)},
		{"DebugHeaders", c.DebugHeaders, false},
		{"AuthURL", c.AuthURL, "http://system_manager:10000/api"},
		{"AdminAuth", c.AdminAuth, AdminAuthIntrospect},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %v, want %v", ch.name, ch.got, ch.want)
		}
	}
	if len(c.Warnings()) != 0 {
		t.Errorf("default warnings %q", c.Warnings())
	}
}

func TestLoadParsesEverything(t *testing.T) {
	c, err := LoadFrom(with(base(),
		"OOA_LOG_LEVEL", "debug",
		"OOA_PUBLIC_URL", "https://llm.example.org",
		"OOA_ENGINE_PORT_RANGE", "20000-20009",
		"OOA_CLUSTER_ABSTRACTOR_OVERRIDES", "c1=http://10.0.0.1:11012,c2=https://ra.example/base/",
		"OOA_POLL_INTERVAL", "1s",
		"OOA_MAX_BODY_BYTES", "1048576",
		"OOA_DEBUG_HEADERS", "true",
		"OOA_ADMIN_AUTH", "none",
	))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.LogLevel != slog.LevelDebug || c.PollInterval != time.Second || !c.DebugHeaders {
		t.Errorf("config = %+v", c)
	}
	if c.MaxBodyBytes != 1<<20 {
		t.Errorf("MaxBodyBytes = %d", c.MaxBodyBytes)
	}
	if c.EnginePortRange.Size() != 10 || !c.EnginePortRange.Contains(20009) || c.EnginePortRange.Contains(20010) {
		t.Errorf("EnginePortRange = %v", c.EnginePortRange)
	}
	if got := c.AbstractorURL("c2", "id2", "1.2.3.4"); got != "https://ra.example/base" {
		t.Errorf("override by name = %q", got)
	}
	if got := c.AbstractorURL("other", "c1", "1.2.3.4"); got != "http://10.0.0.1:11012" {
		t.Errorf("override by id = %q", got)
	}
	if got := c.AbstractorURL("other", "id", "1.2.3.4"); got != "http://1.2.3.4:11012" {
		t.Errorf("default = %q", got)
	}
	if got := c.AbstractorURL("x", "y", "fd00::1"); got != "http://[fd00::1]:11012" {
		t.Errorf("ipv6 = %q", got)
	}
	joined := strings.Join(c.Warnings(), "\n")
	for _, want := range []string{"OOA_ADMIN_AUTH=none", "OOA_DEBUG_HEADERS"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings %q lack %q", joined, want)
		}
	}
}

func TestByteSize(t *testing.T) {
	for in, want := range map[string]ByteSize{"0": 0, "512": 512, "4KiB": 4 << 10, "16MiB": 16 << 20, "2 GiB": 2 << 30} {
		var s ByteSize
		if err := s.UnmarshalText([]byte(in)); err != nil || s != want {
			t.Errorf("%q = %d, %v; want %d", in, s, err, want)
		}
	}
	for _, in := range []string{"", "16MB", "-1", "x"} {
		var s ByteSize
		if err := s.UnmarshalText([]byte(in)); err == nil {
			t.Errorf("%q accepted as %d", in, s)
		}
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"public url bad scheme", with(base(), "OOA_PUBLIC_URL", "ftp://x"), "OOA_PUBLIC_URL"},
		{"no user", with(base(), "OOA_OAKESTRA_USER", ""), "OOA_OAKESTRA_USER"},
		{"same listeners", with(base(), "OOA_ADMIN_LISTEN", ":8080"), "must differ"},
		{"bad admin auth", with(base(), "OOA_ADMIN_AUTH", "jwt"), "OOA_ADMIN_AUTH"},
		{"bad auth url", with(base(), "OOA_AUTH_URL", "system_manager"), "OOA_AUTH_URL"},
		{"bad app name", with(base(), "OOA_APP_NAME", "openai-api"), "OOA_APP_NAME"},
		{"bad override", with(base(), "OOA_CLUSTER_ABSTRACTOR_OVERRIDES", "c1=10.0.0.1"), "OOA_CLUSTER_ABSTRACTOR_OVERRIDES"},
		{"relative mount", with(base(), "OOA_MODEL_MOUNT_PATH", "model"), "OOA_MODEL_MOUNT_PATH"},
		{"tiny interval", with(base(), "OOA_POLL_INTERVAL", "10ms"), "OOA_POLL_INTERVAL"},
		{"no attempts", with(base(), "OOA_MAX_ATTEMPTS", "0"), "OOA_MAX_ATTEMPTS"},
		{"inverted ports", with(base(), "OOA_ENGINE_PORT_RANGE", "19000-18000"), "port range"},
		{"ports not a range", with(base(), "OOA_ENGINE_PORT_RANGE", "18000"), "port range"},
		{"bad duration", with(base(), "OOA_SCHEDULE_TIMEOUT", "soon"), "OOA_SCHEDULE_TIMEOUT"},
		{"bad size", with(base(), "OOA_MAX_BODY_BYTES", "big"), "OOA_MAX_BODY_BYTES"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadFrom(tt.env)
			if err == nil {
				t.Fatal("LoadFrom succeeded")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	_, err := LoadFrom(map[string]string{
		"OOA_APP_NS":          "a.b",
		"OOA_STARTUP_TIMEOUT": "0s",
	})
	if err == nil {
		t.Fatal("LoadFrom succeeded")
	}
	for _, want := range []string{"OOA_OAKESTRA_USER", "OOA_OAKESTRA_PASSWORD", "OOA_APP_NS", "OOA_STARTUP_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}
