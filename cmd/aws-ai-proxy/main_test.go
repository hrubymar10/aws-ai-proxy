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
	}, nil, nil)

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
	}, nil, nil)

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
	}, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/credentials/dev", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

func TestNotifierFiresOnSuccessfulCredentials(t *testing.T) {
	type notice struct {
		profile string
		client  string
	}
	notices := make(chan notice, 1)
	var logBuf bytes.Buffer
	oldLog := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(oldLog)

	handler := newServer(parseAllowedProfiles("dev"), func(string) ([]byte, error) {
		return []byte(`{"Version":1}`), nil
	}, nil, func(profile, client string) {
		notices <- notice{profile: profile, client: client}
	})

	req := httptest.NewRequest(http.MethodGet, "/credentials/dev", nil)
	req.Header.Set(clientHeader, " codex-docker ")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	select {
	case got := <-notices:
		if got != (notice{profile: "dev", client: "codex-docker"}) {
			t.Fatalf("notice = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("notifier was not called")
	}
	if !strings.Contains(logBuf.String(), `OK: "dev" client="codex-docker"`) {
		t.Fatalf("log = %q, want OK line with client", logBuf.String())
	}
}

func TestNotifierDoesNotFireOnDeniedOrFailedCredentials(t *testing.T) {
	t.Run("denied", func(t *testing.T) {
		notices := make(chan struct{}, 1)
		handler := newServer(parseAllowedProfiles("dev"), func(string) ([]byte, error) {
			t.Fatal("exporter should not be called")
			return nil, nil
		}, nil, func(string, string) {
			notices <- struct{}{}
		})

		req := httptest.NewRequest(http.MethodGet, "/credentials/prod", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
		}
		select {
		case <-notices:
			t.Fatal("notifier should not be called")
		default:
		}
	})

	t.Run("export failure", func(t *testing.T) {
		notices := make(chan struct{}, 1)
		handler := newServer(parseAllowedProfiles("dev"), func(string) ([]byte, error) {
			return nil, errors.New("aws failed")
		}, nil, func(string, string) {
			notices <- struct{}{}
		})

		req := httptest.NewRequest(http.MethodGet, "/credentials/dev", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
		}
		select {
		case <-notices:
			t.Fatal("notifier should not be called")
		default:
		}
	})
}

func TestRequestClientPrecedenceAndSanitization(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/credentials/dev", nil)
	req.Header.Set(clientHeader, " explicit\nclient ")
	req.Header.Set("User-Agent", "ua")
	if got := requestClient(req); got != "explicitclient" {
		t.Fatalf("explicit client = %q", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/credentials/dev", nil)
	req.Header.Set("User-Agent", " user-agent ")
	if got := requestClient(req); got != "user-agent" {
		t.Fatalf("user-agent client = %q", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/credentials/dev", nil)
	if got := requestClient(req); got != "" {
		t.Fatalf("empty client = %q", got)
	}
}

func TestSanitizeNotifyValueCapsRunesAndDropsControlChars(t *testing.T) {
	long := strings.Repeat("å", maxNotifyValueRunes+5)
	got := sanitizeNotifyValue(" \n" + long + "\t ")
	if len([]rune(got)) != maxNotifyValueRunes {
		t.Fatalf("sanitized rune count = %d, want %d", len([]rune(got)), maxNotifyValueRunes)
	}
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("sanitized value still contains control chars: %q", got)
	}
}

func TestNotifyDeduperThrottlesPerClientAndProfile(t *testing.T) {
	d := newNotifyDeduper(5 * time.Minute)
	base := time.Unix(1_700_000_000, 0)
	now := base
	d.now = func() time.Time { return now }

	if !d.allow("claude-docker", "dev") {
		t.Fatal("first notification should be allowed")
	}
	if d.allow("claude-docker", "dev") {
		t.Fatal("repeat within window should be suppressed")
	}

	// A different profile for the same client is an independent event.
	if !d.allow("claude-docker", "prod") {
		t.Fatal("different profile should be allowed")
	}
	// A different client is independent too.
	if !d.allow("codex-docker", "dev") {
		t.Fatal("different client should be allowed")
	}

	// Just before the window elapses: still suppressed.
	now = base.Add(5*time.Minute - time.Nanosecond)
	if d.allow("claude-docker", "dev") {
		t.Fatal("still within window should be suppressed")
	}
	// After the window: allowed again.
	now = base.Add(5 * time.Minute)
	if !d.allow("claude-docker", "dev") {
		t.Fatal("after window should be allowed again")
	}
}

func TestNotifyDeduperDisabledWhenWindowNonPositive(t *testing.T) {
	for _, w := range []time.Duration{0, -time.Minute} {
		d := newNotifyDeduper(w)
		for i := range 5 {
			if !d.allow("claude-docker", "dev") {
				t.Fatalf("window %s should disable dedup (call %d suppressed)", w, i)
			}
		}
	}
}

func TestNotificationDedupWindowConfig(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		unsetEnv(t, envNotificationDedupWindow)
		if got := notificationDedupWindow(); got != defaultNotificationDedupWindow {
			t.Fatalf("default = %s, want %s", got, defaultNotificationDedupWindow)
		}
	})
	t.Run("valid duration honored", func(t *testing.T) {
		t.Setenv(envNotificationDedupWindow, "90s")
		if got := notificationDedupWindow(); got != 90*time.Second {
			t.Fatalf("got %s, want 90s", got)
		}
	})
	t.Run("zero disables", func(t *testing.T) {
		t.Setenv(envNotificationDedupWindow, "0")
		if got := notificationDedupWindow(); got != 0 {
			t.Fatalf("got %s, want 0", got)
		}
	})
	t.Run("negative treated as disabled", func(t *testing.T) {
		t.Setenv(envNotificationDedupWindow, "-5m")
		if got := notificationDedupWindow(); got != 0 {
			t.Fatalf("got %s, want 0", got)
		}
	})
	t.Run("invalid falls back to default", func(t *testing.T) {
		t.Setenv(envNotificationDedupWindow, "not-a-duration")
		if got := notificationDedupWindow(); got != defaultNotificationDedupWindow {
			t.Fatalf("got %s, want %s", got, defaultNotificationDedupWindow)
		}
	})
}

func TestHealth(t *testing.T) {
	handler := newServer(parseAllowedProfiles("dev"), nil, nil, nil)
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
	handler := newServer(parseAllowedProfiles("dev"), nil, nil, nil)
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
	}, nil)
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

func TestProfileListRegionPrecedence(t *testing.T) {
	allowed := parseAllowedProfiles("inline:eu-west-1, lookup, empty")
	lookups := map[string]string{"inline": "us-east-1", "lookup": "ap-southeast-2"}
	var lookedUp []string
	got := profileList(allowed, func(profile string) string {
		lookedUp = append(lookedUp, profile)
		return lookups[profile]
	})

	want := []profileConfig{
		{Name: "empty", Region: ""},
		{Name: "inline", Region: "eu-west-1"},
		{Name: "lookup", Region: "ap-southeast-2"},
	}
	if len(got) != len(want) {
		t.Fatalf("profiles = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("profiles[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
	if strings.Join(lookedUp, ",") != "empty,lookup" {
		t.Fatalf("lookup calls = %#v, want empty and lookup only", lookedUp)
	}
}

func TestParseAllowedProfilesNamesOnly(t *testing.T) {
	allowed := parseAllowedProfiles(" dev,prod ,,")
	if got := allowed["dev"]; got != "" {
		t.Fatalf("dev region = %q, want empty", got)
	}
	if got := allowed["prod"]; got != "" {
		t.Fatalf("prod region = %q, want empty", got)
	}
	if len(allowed) != 2 {
		t.Fatalf("allowed = %#v, want 2 entries", allowed)
	}
}

func TestParseAllowedProfilesKeepsRegionSuffix(t *testing.T) {
	allowed := parseAllowedProfiles("dev:us-east-1, prod: eu-west-1 ")
	if got := allowed["dev"]; got != "us-east-1" {
		t.Fatalf("dev region = %q, want us-east-1", got)
	}
	if got := allowed["prod"]; got != "eu-west-1" {
		t.Fatalf("prod region = %q, want eu-west-1", got)
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

func TestNotificationsEnabledDefaultAndFalsey(t *testing.T) {
	unsetEnv(t, envNotifications)
	if !notificationsEnabled() {
		t.Fatal("notifications should be enabled by default")
	}
	for _, val := range []string{"false", "FALSE", "0", "no", "off", " Off "} {
		t.Run(val, func(t *testing.T) {
			t.Setenv(envNotifications, val)
			if notificationsEnabled() {
				t.Fatalf("%q should disable notifications", val)
			}
		})
	}
}

func TestNotificationCommand(t *testing.T) {
	t.Run("darwin", func(t *testing.T) {
		name, args, err := notificationCommandWithLookPath("darwin", "dev", "codex", nil)
		if err != nil {
			t.Fatal(err)
		}
		if name != "osascript" {
			t.Fatalf("name = %q, want osascript", name)
		}
		if len(args) != 2 || args[0] != "-e" {
			t.Fatalf("args = %#v, want osascript expression", args)
		}
		want := `display notification "Profile \"dev\" was requested by codex" with title "aws-ai-proxy"`
		if args[1] != want {
			t.Fatalf("osascript expression = %q, want %q", args[1], want)
		}
	})

	t.Run("linux", func(t *testing.T) {
		name, args, err := notificationCommandWithLookPath("linux", "dev", "codex", func(binary string) (string, error) {
			if binary != "notify-send" {
				t.Fatalf("lookPath binary = %q", binary)
			}
			return "/usr/bin/notify-send", nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if name != "/usr/bin/notify-send" {
			t.Fatalf("name = %q, want notify-send path", name)
		}
		want := []string{notificationTitle, `Profile "dev" was requested by codex`}
		if len(args) != len(want) {
			t.Fatalf("args = %#v, want %#v", args, want)
		}
		for i := range want {
			if args[i] != want[i] {
				t.Fatalf("args[%d] = %q, want %q", i, args[i], want[i])
			}
		}
	})

	t.Run("linux missing", func(t *testing.T) {
		_, _, err := notificationCommandWithLookPath("linux", "dev", "", func(string) (string, error) {
			return "", errors.New("missing")
		})
		if err == nil || !strings.Contains(err.Error(), "notify-send not found") {
			t.Fatalf("error = %v, want notify-send not found", err)
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		_, _, err := notificationCommandWithLookPath("windows", "dev", "", nil)
		if err == nil || !strings.Contains(err.Error(), "unsupported OS") {
			t.Fatalf("error = %v, want unsupported OS", err)
		}
	})
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

func TestLoadConfigFileTreatsEmptyOptionalAWSPathsAsUnset(t *testing.T) {
	unsetEnv(t, envAWSCLIBinaryPath)
	unsetEnv(t, envAWSConfigPath)
	configPath := filepath.Join(t.TempDir(), "config")
	data := []byte(envAWSCLIBinaryPath + "=\n" + envAWSConfigPath + "=\n")
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadConfigFile(configPath); err != nil {
		t.Fatal(err)
	}
	if _, ok := os.LookupEnv(envAWSCLIBinaryPath); ok {
		t.Fatalf("empty %s line should not set env", envAWSCLIBinaryPath)
	}
	if _, ok := os.LookupEnv(envAWSConfigPath); ok {
		t.Fatalf("empty %s line should not set env", envAWSConfigPath)
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
	for _, key := range []string{"AWS_AI_PROXY_PROFILES", "AWS_AI_PROXY_BIND", "AWS_AI_PROXY_ALLOW", "AWS_AI_PROXY_ACCESS_LOGS_ENABLED", envNotifications, envAWSCLIBinaryPath, envAWSConfigPath} {
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
	if !strings.Contains(text, envNotifications+"=true") {
		t.Fatalf("notification default was not appended:\n%s", text)
	}
	if !strings.Contains(text, envAWSCLIBinaryPath+"=") {
		t.Fatalf("aws cli path default was not appended:\n%s", text)
	}
	if !strings.Contains(text, envAWSConfigPath+"=") {
		t.Fatalf("aws config path default was not appended:\n%s", text)
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

func TestResolveAWSCLIPathUsesConfiguredExecutable(t *testing.T) {
	t.Setenv(envAWSCLIBinaryPath, fakeExecutable(t, t.TempDir(), "aws-configured", "#!/bin/sh\n"))

	got, err := resolveAWSCLIPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != os.Getenv(envAWSCLIBinaryPath) {
		t.Fatalf("path = %q, want configured path", got)
	}
}

func TestResolveAWSCLIPathExpandsHomeForConfiguredExecutable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	awsPath := fakeExecutable(t, filepath.Join(home, "bin"), "aws", "#!/bin/sh\n")
	t.Setenv(envAWSCLIBinaryPath, "~/bin/aws")

	got, err := resolveAWSCLIPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != awsPath {
		t.Fatalf("path = %q, want expanded configured path %q", got, awsPath)
	}
}

func TestResolveAWSCLIPathRejectsInvalidConfiguredPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aws")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envAWSCLIBinaryPath, path)

	if _, err := resolveAWSCLIPath(); err == nil || !strings.Contains(err.Error(), envAWSCLIBinaryPath) {
		t.Fatalf("error = %v, want configured path error", err)
	}
}

func TestResolveAWSCLIPathUsesLookPath(t *testing.T) {
	unsetEnv(t, envAWSCLIBinaryPath)
	dir := t.TempDir()
	path := fakeExecutable(t, dir, "aws", "#!/bin/sh\n")
	t.Setenv("PATH", dir)
	withCommonAWSCLIPaths(t, nil)

	got, err := resolveAWSCLIPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("path = %q, want %q", got, path)
	}
}

func TestResolveAWSCLIPathUsesCommonPathFallback(t *testing.T) {
	unsetEnv(t, envAWSCLIBinaryPath)
	t.Setenv("PATH", t.TempDir())
	path := fakeExecutable(t, t.TempDir(), "aws-common", "#!/bin/sh\n")
	withCommonAWSCLIPaths(t, []string{filepath.Join(t.TempDir(), "missing"), path})

	got, err := resolveAWSCLIPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("path = %q, want %q", got, path)
	}
}

func TestResolveAWSCLIPathReportsAttemptsWhenNotFound(t *testing.T) {
	unsetEnv(t, envAWSCLIBinaryPath)
	t.Setenv("PATH", t.TempDir())
	missing := filepath.Join(t.TempDir(), "missing-aws")
	withCommonAWSCLIPaths(t, []string{missing})

	_, err := resolveAWSCLIPath()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "PATH:aws") || !strings.Contains(err.Error(), missing) {
		t.Fatalf("error = %v, want attempted paths", err)
	}
}

func TestAWSSubprocessesUseConfiguredAWSConfigFile(t *testing.T) {
	dir := t.TempDir()
	awsPath := fakeExecutable(t, dir, "aws", `#!/bin/sh
if [ "$1" = "configure" ] && [ "$2" = "get" ]; then
  printf "%s\n" "$AWS_CONFIG_FILE"
  exit 0
fi
if [ "$1" = "configure" ] && [ "$2" = "export-credentials" ]; then
  printf '{"config":"%s"}' "$AWS_CONFIG_FILE"
  exit 0
fi
exit 9
`)
	configPath := filepath.Join(t.TempDir(), "aws-config")
	t.Setenv(envAWSCLIBinaryPath, awsPath)
	t.Setenv(envAWSConfigPath, configPath)

	if got := lookupRegion("dev"); got != configPath {
		t.Fatalf("lookupRegion = %q, want config path", got)
	}
	out, err := exportCredentials("dev")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), configPath) {
		t.Fatalf("credentials output = %q, want config path", out)
	}
}

func TestExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := []struct {
		in   string
		want string
	}{
		{"~", home},
		{"~/x", filepath.Join(home, "x")},
		{"/abs", "/abs"},
		{"rel", "rel"},
		{"", ""},
		{"~other/config", "~other/config"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := expandHome(tc.in); got != tc.want {
				t.Fatalf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAWSConfigFileExpandsHomeBeforeSubprocess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	awsPath := fakeExecutable(t, t.TempDir(), "aws", `#!/bin/sh
printf "%s\n" "$AWS_CONFIG_FILE"
`)
	t.Setenv(envAWSCLIBinaryPath, awsPath)
	t.Setenv(envAWSConfigPath, "~/.aws/config")

	if got := lookupRegion("dev"); got != filepath.Join(home, ".aws", "config") {
		t.Fatalf("lookupRegion = %q, want expanded config path", got)
	}
}

func TestAWSConfigFileResolution(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		unsetEnv(t, envAWSConfigPath)
		if got := awsConfigFile(); got != "" {
			t.Fatalf("awsConfigFile = %q, want empty", got)
		}
	})

	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(envAWSConfigPath, dir)
		if got := awsConfigFile(); got != filepath.Join(dir, "config") {
			t.Fatalf("awsConfigFile = %q, want dir/config", got)
		}
	})

	t.Run("file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config")
		if err := os.WriteFile(path, []byte("[default]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(envAWSConfigPath, path)
		if got := awsConfigFile(); got != path {
			t.Fatalf("awsConfigFile = %q, want file path", got)
		}
	})

	t.Run("tilde directory", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		awsDir := filepath.Join(home, ".aws")
		if err := os.MkdirAll(awsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(envAWSConfigPath, "~/.aws")
		if got := awsConfigFile(); got != filepath.Join(awsDir, "config") {
			t.Fatalf("awsConfigFile = %q, want expanded dir/config", got)
		}
	})
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func fakeExecutable(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func withCommonAWSCLIPaths(t *testing.T, paths []string) {
	t.Helper()
	orig := commonAWSCLIPaths
	commonAWSCLIPaths = paths
	t.Cleanup(func() { commonAWSCLIPaths = orig })
}
