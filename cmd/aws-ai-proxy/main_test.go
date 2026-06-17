package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunCommandHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{nil, []string{"help"}, []string{"-h"}, []string{"--help"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := runCommand(args, &out, &errOut)
			if code != 0 {
				t.Fatalf("code = %d, want 0", code)
			}
			if !strings.Contains(out.String(), "Usage: aws-ai-proxy <command>") {
				t.Fatalf("help output missing usage: %q", out.String())
			}
			if errOut.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", errOut.String())
			}
		})
	}

	var out, errOut bytes.Buffer
	code := runCommand([]string{"version"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("version code = %d, want 0", code)
	}
	if got := out.String(); got != "aws-ai-proxy "+version+"\n" {
		t.Fatalf("version output = %q", got)
	}
}

func TestRunCommandUnknown(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runCommand([]string{"bogus"}, &out, &errOut)
	if code == 0 {
		t.Fatal("unknown command returned success")
	}
	if !strings.Contains(errOut.String(), "unknown command") || !strings.Contains(errOut.String(), "Usage:") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
}

func TestRunCommandStatusStopped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AWS_AI_PROXY_BIND", "127.0.0.1:1")

	var out, errOut bytes.Buffer
	code := runCommand([]string{"status"}, &out, &errOut)
	if code == 0 {
		t.Fatal("status stopped returned success")
	}
	if !strings.Contains(out.String(), "stopped") {
		t.Fatalf("stdout = %q, want stopped", out.String())
	}
}

func TestPIDFileLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".aws-ai-proxy", "aws-ai-proxy.pid")
	if err := writePIDFile(path, 12345); err != nil {
		t.Fatal(err)
	}
	got, err := readPIDFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != 12345 {
		t.Fatalf("pid = %d, want 12345", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("pid file mode = %o, want 600", mode)
	}
}

func TestAllowedProfileReturnsExportedCredentials(t *testing.T) {
	allowed := parseAllowedProfiles("dev")
	handler := newServer(allowed, func(profile string) ([]byte, error) {
		if profile != "dev" {
			t.Fatalf("unexpected profile %q", profile)
		}
		return []byte(`{"Version":1}`), nil
	}, nil)

	req := httptest.NewRequest(http.MethodGet, "/credentials/dev", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	if strings.TrimSpace(rec.Body.String()) != `{"Version":1}` {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestDisallowedProfileIsForbidden(t *testing.T) {
	handler := newServer(parseAllowedProfiles("dev"), func(string) ([]byte, error) {
		t.Fatal("exporter should not be called")
		return nil, nil
	}, nil)

	req := httptest.NewRequest(http.MethodGet, "/credentials/prod", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestExporterFailureIsBadGateway(t *testing.T) {
	handler := newServer(parseAllowedProfiles("dev"), func(string) ([]byte, error) {
		return nil, errors.New("aws failed")
	}, nil)

	req := httptest.NewRequest(http.MethodGet, "/credentials/dev", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestHealth(t *testing.T) {
	handler := newServer(parseAllowedProfiles("dev"), nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != "ok\n" {
		t.Fatalf("body = %q, want ok newline", got)
	}
}

func TestVersion(t *testing.T) {
	handler := newServer(parseAllowedProfiles("dev"), nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != version+"\n" {
		t.Fatalf("body = %q, want version newline", got)
	}
}

func TestProfiles(t *testing.T) {
	regions := map[string]string{"dev": "us-east-1", "prod": ""}
	handler := newServer(parseAllowedProfiles("prod, dev"), nil, func(profile string) string {
		return regions[profile]
	})
	req := httptest.NewRequest(http.MethodGet, "/profiles", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	var got []profileConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []profileConfig{
		{Name: "dev", Region: "us-east-1"},
		{Name: "prod", Region: ""},
	}
	if len(got) != len(want) {
		t.Fatalf("profiles = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("profiles[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestParseAllowedProfilesNamesOnly(t *testing.T) {
	allowed := parseAllowedProfiles(" dev,prod ,,")
	if _, ok := allowed["dev"]; !ok {
		t.Fatal("dev missing")
	}
	if _, ok := allowed["prod"]; !ok {
		t.Fatal("prod missing")
	}
	if len(allowed) != 2 {
		t.Fatalf("allowed = %#v, want 2 entries", allowed)
	}
}

func TestParseAllowedProfilesToleratesLegacyRegionSuffix(t *testing.T) {
	allowed := parseAllowedProfiles("dev:us-east-1,prod:eu-west-1")
	if _, ok := allowed["dev"]; !ok {
		t.Fatal("dev missing")
	}
	if _, ok := allowed["prod"]; !ok {
		t.Fatal("prod missing")
	}
	if len(allowed) != 2 {
		t.Fatalf("allowed = %#v, want 2 entries", allowed)
	}
}

func TestResolveBindAddrDefault(t *testing.T) {
	t.Setenv("AWS_AI_PROXY_BIND", "")
	got, err := resolveBindAddr()
	if err != nil {
		t.Fatalf("default resolution should not error: %v", err)
	}
	if got != defaultBindAddr {
		t.Errorf("expected %q, got %q", defaultBindAddr, got)
	}
}

func TestResolveBindAddrOverrideHonoured(t *testing.T) {
	for _, val := range []string{"0.0.0.0:9998", "172.28.47.1:9998", "[::1]:9998", "[::]:9998"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("AWS_AI_PROXY_BIND", val)
			got, err := resolveBindAddr()
			if err != nil {
				t.Fatalf("%q should be accepted: %v", val, err)
			}
			if got != val {
				t.Errorf("expected verbatim %q, got %q", val, got)
			}
		})
	}
}

func TestResolveBindAddrRejectsInvalid(t *testing.T) {
	cases := []struct{ name, val string }{
		{"empty host", ":9998"},
		{"hostname", "localhost:9998"},
		{"bad port", "127.0.0.1:abc"},
		{"no port", "garbage"},
		{"port out of range", "127.0.0.1:99999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AWS_AI_PROXY_BIND", tc.val)
			if _, err := resolveBindAddr(); err == nil {
				t.Errorf("expected error for %q, got nil", tc.val)
			}
		})
	}
}

func TestResolveAllowDefault(t *testing.T) {
	t.Setenv("AWS_AI_PROXY_ALLOW", "")
	prefixes, err := resolveAllow()
	if err != nil {
		t.Fatalf("default resolution should not error: %v", err)
	}
	want := []string{"127.0.0.0/8", "::1/128"}
	if len(prefixes) != len(want) {
		t.Fatalf("expected %d prefixes, got %d (%v)", len(want), len(prefixes), prefixes)
	}
	for i, w := range want {
		if prefixes[i].String() != w {
			t.Errorf("prefix[%d]: expected %q, got %q", i, w, prefixes[i].String())
		}
	}
}

func TestResolveAllowMixedIPAndCIDR(t *testing.T) {
	t.Setenv("AWS_AI_PROXY_ALLOW", "127.0.0.1, 172.28.47.0/24,172.28.47.62")
	prefixes, err := resolveAllow()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"127.0.0.1/32", "172.28.47.0/24", "172.28.47.62/32"}
	if len(prefixes) != len(want) {
		t.Fatalf("expected %d prefixes, got %d (%v)", len(want), len(prefixes), prefixes)
	}
	for i, w := range want {
		if prefixes[i].String() != w {
			t.Errorf("prefix[%d]: expected %q, got %q", i, w, prefixes[i].String())
		}
	}
}

func TestResolveAllowRejectsInvalid(t *testing.T) {
	cases := []struct{ name, val string }{
		{"trailing comma", "127.0.0.1,"},
		{"leading comma", ",127.0.0.1"},
		{"empty middle", "127.0.0.1,,127.0.0.2"},
		{"hostname", "localhost"},
		{"malformed CIDR", "172.28.47.0/33"},
		{"garbage", "not-an-ip"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AWS_AI_PROXY_ALLOW", tc.val)
			if _, err := resolveAllow(); err == nil {
				t.Errorf("expected error for %q, got nil", tc.val)
			}
		})
	}
}

func TestAllowMiddleware(t *testing.T) {
	allow := []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("172.28.47.0/24"),
		netip.MustParsePrefix("::1/128"),
	}
	handler := allowMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), allow)

	cases := []struct {
		name       string
		remoteAddr string
		wantStatus int
	}{
		{"loopback v4", "127.0.0.1:54321", http.StatusOK},
		{"loopback v4 in /8 range", "127.5.6.7:54321", http.StatusOK},
		{"vpn v4 in /24", "172.28.47.62:54321", http.StatusOK},
		{"non-allowlisted v4", "10.0.0.1:54321", http.StatusForbidden},
		{"v4-mapped v6 loopback", "[::ffff:127.0.0.1]:54321", http.StatusOK},
		{"v4-mapped v6 non-allowed", "[::ffff:10.0.0.1]:54321", http.StatusForbidden},
		{"v6 loopback", "[::1]:54321", http.StatusOK},
		{"unparseable", "notanip", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			req.RemoteAddr = tc.remoteAddr
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tc.wantStatus {
				t.Errorf("RemoteAddr %q: status got %d, want %d", tc.remoteAddr, rr.Code, tc.wantStatus)
			}
		})
	}
}

func TestAccessLogMiddlewareWritesRequestLine(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := accessLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}), logger)

	req := httptest.NewRequest(http.MethodGet, "/credentials/dev", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	fields := strings.Fields(strings.TrimSpace(buf.String()))
	if len(fields) != 5 {
		t.Fatalf("log fields = %#v, want 5 fields from %q", fields, buf.String())
	}
	if _, err := time.Parse(time.RFC3339, fields[0]); err != nil {
		t.Fatalf("timestamp = %q: %v", fields[0], err)
	}
	if fields[1] != "127.0.0.1" || fields[2] != http.MethodGet || fields[3] != "/credentials/dev" || fields[4] != "403" {
		t.Fatalf("log line = %q", buf.String())
	}
}

func TestAccessLogsEnabledDefaultAndFalsey(t *testing.T) {
	unsetEnv(t, "AWS_AI_PROXY_ACCESS_LOGS_ENABLED")
	if !accessLogsEnabled() {
		t.Fatal("access logs should be enabled by default")
	}
	for _, val := range []string{"false", "FALSE", "0", "no", "off", " Off "} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("AWS_AI_PROXY_ACCESS_LOGS_ENABLED", val)
			if accessLogsEnabled() {
				t.Fatalf("%q should disable access logs", val)
			}
		})
	}
}

func TestLoadConfigFileMissingIsNoop(t *testing.T) {
	if err := loadConfigFile(filepath.Join(t.TempDir(), "missing.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigFileSetsMissingEnv(t *testing.T) {
	unsetEnv(t, "AWS_AI_PROXY_PROFILES")
	unsetEnv(t, "AWS_AI_PROXY_BIND")
	unsetEnv(t, "AWS_AI_PROXY_ALLOW")
	unsetEnv(t, "UNRELATED_SETTING")

	configPath := filepath.Join(t.TempDir(), "config")
	data := []byte(`
# env-file values mirror AWS_AI_PROXY_* env vars
AWS_AI_PROXY_BIND="127.0.0.1:19998"
AWS_AI_PROXY_ALLOW='127.0.0.0/8,::1/128'
AWS_AI_PROXY_PROFILES=dev,prod
UNRELATED_SETTING=preserved
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := loadConfigFile(configPath); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("AWS_AI_PROXY_BIND"); got != "127.0.0.1:19998" {
		t.Fatalf("bind = %q", got)
	}
	if got := os.Getenv("AWS_AI_PROXY_ALLOW"); got != "127.0.0.0/8,::1/128" {
		t.Fatalf("allow = %q", got)
	}
	if got := os.Getenv("AWS_AI_PROXY_PROFILES"); got != "dev,prod" {
		t.Fatalf("profiles = %q", got)
	}
	if got := os.Getenv("UNRELATED_SETTING"); got != "preserved" {
		t.Fatalf("unrelated = %q", got)
	}
}

func TestLoadConfigFileDoesNotOverrideEnv(t *testing.T) {
	unsetEnv(t, "AWS_AI_PROXY_BIND")
	t.Setenv("AWS_AI_PROXY_PROFILES", "env")

	configPath := filepath.Join(t.TempDir(), "config")
	data := []byte("AWS_AI_PROXY_PROFILES=file\nAWS_AI_PROXY_BIND=127.0.0.1:19998\n")
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := loadConfigFile(configPath); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("AWS_AI_PROXY_PROFILES"); got != "env" {
		t.Fatalf("profiles = %q", got)
	}
	if got := os.Getenv("AWS_AI_PROXY_BIND"); got != "127.0.0.1:19998" {
		t.Fatalf("bind = %q", got)
	}
}

func TestLoadConfigFileTreatsEmptyProfilesAsUnset(t *testing.T) {
	unsetEnv(t, "AWS_AI_PROXY_PROFILES")
	configPath := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(configPath, []byte("AWS_AI_PROXY_PROFILES=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfigFile(configPath); err != nil {
		t.Fatal(err)
	}
	if _, ok := os.LookupEnv("AWS_AI_PROXY_PROFILES"); ok {
		t.Fatal("empty profiles line should not set AWS_AI_PROXY_PROFILES")
	}
}

func TestLoadConfigFileRejectsMalformedLine(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(configPath, []byte("AWS_AI_PROXY_PROFILES dev us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfigFile(configPath); err == nil || !strings.Contains(err.Error(), "expected KEY=VALUE") {
		t.Fatalf("error = %v, want expected KEY=VALUE", err)
	}
}

func TestLoadConfigFileRejectsEmptyKey(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(configPath, []byte("=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfigFile(configPath); err == nil || !strings.Contains(err.Error(), "empty key") {
		t.Fatalf("error = %v, want empty key", err)
	}
}

func TestEnsureConfigFileCreatesTemplate(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), ".aws-ai-proxy", "config")
	if err := ensureConfigFile(configPath); err != nil {
		t.Fatal(err)
	}

	dirInfo, err := os.Stat(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir mode = %o, want 700", got)
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, key := range []string{"AWS_AI_PROXY_PROFILES", "AWS_AI_PROXY_BIND", "AWS_AI_PROXY_ALLOW", "AWS_AI_PROXY_ACCESS_LOGS_ENABLED"} {
		if !strings.Contains(text, key+"=") {
			t.Fatalf("template missing %s line:\n%s", key, text)
		}
		if strings.Contains(text, "# "+key+"=") {
			t.Fatalf("template key should be uncommented for %s:\n%s", key, text)
		}
	}
	if !strings.Contains(text, "AWS_AI_PROXY_PROFILES=\n") {
		t.Fatalf("profiles default should be empty:\n%s", text)
	}
}

func TestEnsureConfigFileAppendsOnlyMissingDefaults(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), ".aws-ai-proxy", "config")
	original := "AWS_AI_PROXY_PROFILES=custom\n# AWS_AI_PROXY_BIND=127.0.0.1:19998\n"
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ensureConfigFile(configPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.HasPrefix(text, original) {
		t.Fatalf("existing lines changed:\n%s", text)
	}
	if strings.Count(text, "AWS_AI_PROXY_PROFILES=") != 1 {
		t.Fatalf("profiles default should not be appended:\n%s", text)
	}
	if !strings.Contains(text, "AWS_AI_PROXY_BIND="+defaultBindAddr) {
		t.Fatalf("bind default should be appended as an active key:\n%s", text)
	}
	if !strings.Contains(text, "AWS_AI_PROXY_ALLOW="+defaultAllowList) {
		t.Fatalf("allow default was not appended:\n%s", text)
	}
	if !strings.Contains(text, "AWS_AI_PROXY_ACCESS_LOGS_ENABLED=true") {
		t.Fatalf("access log default was not appended:\n%s", text)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}
}

func TestEnsureConfigFileIsIdempotent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), ".aws-ai-proxy", "config")
	if err := ensureConfigFile(configPath); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureConfigFile(configPath); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("second ensure changed file:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}
