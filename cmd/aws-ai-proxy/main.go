package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type credentialExporter func(profile string) ([]byte, error)
type regionLookup func(profile string) string

type profileConfig struct {
	Name   string `json:"name"`
	Region string `json:"region"`
}

const (
	defaultBindAddr  = "127.0.0.1:9998"
	defaultAllowList = "127.0.0.0/8,::1/128"

	envAWSCLIBinaryPath = "AWS_AI_PROXY_AWS_CLI_BINARY_PATH"
	envAWSConfigPath    = "AWS_AI_PROXY_AWS_CONFIG_PATH"
)

var version = "dev"

var commonAWSCLIPaths = []string{
	"/opt/homebrew/bin/aws",
	"/usr/local/bin/aws",
	"/usr/bin/aws",
}

var configDefaults = []struct {
	key     string
	comment string
	value   string
}{
	{
		key:     "AWS_AI_PROXY_PROFILES",
		comment: "Required: comma-separated profile names",
		value:   "",
	},
	{
		key:     "AWS_AI_PROXY_BIND",
		comment: "Bind address (host must be an IP literal)",
		value:   defaultBindAddr,
	},
	{
		key:     "AWS_AI_PROXY_ALLOW",
		comment: "Source IP/CIDR allowlist",
		value:   defaultAllowList,
	},
	{
		key:     "AWS_AI_PROXY_ACCESS_LOGS_ENABLED",
		comment: "Access logging to ~/.aws-ai-proxy/access.log",
		value:   "true",
	},
	{
		key:     envAWSCLIBinaryPath,
		comment: "Optional absolute path to the aws CLI binary",
		value:   "",
	},
	{
		key:     envAWSConfigPath,
		comment: "Optional path passed to aws subprocesses as AWS_CONFIG_FILE",
		value:   "",
	},
}

func main() {
	os.Exit(runCommand(os.Args[1:], os.Stdout, os.Stderr))
}

func runCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stdout)
		return 0
	}

	switch args[0] {
	case "serve":
		if err := serve(); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "status":
		if err := status(stdout); err != nil {
			fmt.Fprintln(stdout, err)
			return 1
		}
		return 0
	case "stop":
		if err := stop(stdout); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "version", "--version":
		fmt.Fprintf(stdout, "aws-ai-proxy %s\n", version)
		return 0
	case "help", "-h", "--help":
		printHelp(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printHelp(stderr)
		return 2
	}
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: aws-ai-proxy <command>

Commands:
  serve      Run the HTTP credential proxy
  status     Show whether the proxy is responding
  stop       Stop the process recorded in the PID file
  version    Print version information
  help       Show this help

Configuration:
  AWS_AI_PROXY_PROFILES   Required comma-separated profile names
  AWS_AI_PROXY_BIND       Bind address, default 127.0.0.1:9998
  AWS_AI_PROXY_ALLOW      Source IP/CIDR allowlist, default 127.0.0.0/8,::1/128
  AWS_AI_PROXY_ACCESS_LOGS_ENABLED
                          Access logging, default true; false/0/no/off disables
  AWS_AI_PROXY_AWS_CLI_BINARY_PATH
                          Optional absolute path to the aws CLI binary
  AWS_AI_PROXY_AWS_CONFIG_PATH
                          Optional path passed to aws as AWS_CONFIG_FILE

If an environment variable is unset, aws-ai-proxy reads ~/.aws-ai-proxy/config
using KEY=VALUE lines. Environment variables override file values per field.
`)
}

func serve() error {
	if err := loadConfig(); err != nil {
		return err
	}
	if closeErrorLog, err := openErrorLogger(os.Stderr); err != nil {
		log.Printf("WARN: error logging disabled: %v", err)
	} else {
		defer closeErrorLog()
	}

	profilesRaw := os.Getenv("AWS_AI_PROXY_PROFILES")
	if profilesRaw == "" {
		return fmt.Errorf("AWS_AI_PROXY_PROFILES is required (comma-separated profile names)")
	}

	allowed := parseAllowedProfiles(profilesRaw)
	if len(allowed) == 0 {
		return fmt.Errorf("AWS_AI_PROXY_PROFILES did not contain any profiles")
	}

	addr, err := resolveBindAddr()
	if err != nil {
		return err
	}
	allow, err := resolveAllow()
	if err != nil {
		return err
	}

	names := profileNames(allowed)
	log.Printf("AWS AI proxy %s on http://%s (allow: %v, profiles: %s)", version, addr, allow, strings.Join(names, ", "))
	host, _, _ := net.SplitHostPort(addr)
	if ip, err := netip.ParseAddr(host); err == nil && !ip.IsLoopback() {
		log.Printf("WARN: bound to %s - anyone reachable on this interface can attempt to request credentials (AWS_AI_PROXY_ALLOW gates by source IP)", host)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer ln.Close()

	pidPath, err := pidFilePath()
	if err != nil {
		log.Printf("WARN: could not resolve PID file: %v", err)
	} else if err := writePIDFile(pidPath, os.Getpid()); err != nil {
		log.Printf("WARN: could not write PID file %s: %v", pidPath, err)
	} else {
		defer os.Remove(pidPath)
	}

	handler := allowMiddleware(newServer(allowed, exportCredentials, lookupRegion), allow)
	if accessLogsEnabled() {
		if accessLogger, closeLog, err := openAccessLogger(); err != nil {
			log.Printf("WARN: access logging disabled: %v", err)
		} else {
			defer closeLog()
			handler = accessLogMiddleware(handler, accessLogger)
		}
	}

	server := &http.Server{Handler: handler}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(ln)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
		log.Printf("received %s, shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			return err
		}
		if err := <-errCh; err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func status(w io.Writer) error {
	if err := loadConfig(); err != nil {
		return err
	}
	addr, err := resolveBindAddr()
	if err != nil {
		return err
	}
	baseURL, err := localBaseURL(addr)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: time.Second}
	if _, err := getText(client, baseURL+"/health"); err != nil {
		return fmt.Errorf("stopped")
	}

	pidText := "unknown"
	if pidPath, err := pidFilePath(); err == nil {
		if pid, err := readPIDFile(pidPath); err == nil {
			pidText = strconv.Itoa(pid)
		}
	}

	versionText, _ := getText(client, baseURL+"/version")
	profilesText := "unknown"
	if profiles, err := getProfiles(client, baseURL+"/profiles"); err == nil {
		profilesText = strings.Join(profileNames(profiles), ",")
	}
	fmt.Fprintf(w, "running (PID %s, version %s, addr %s, profiles %s)\n", pidText, strings.TrimSpace(versionText), baseURL, profilesText)
	return nil
}

func stop(w io.Writer) error {
	pidPath, err := pidFilePath()
	if err != nil {
		return err
	}
	pid, err := readPIDFile(pidPath)
	if err != nil {
		fmt.Fprintln(w, "not running")
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		_ = os.Remove(pidPath)
		fmt.Fprintf(w, "not running (removed stale PID %d)\n", pid)
		return nil
	}
	fmt.Fprintf(w, "stopping PID %d\n", pid)
	return nil
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

func accessLogMiddleware(next http.Handler, logger *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusResponseWriter{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		logger.Printf("%s %s %s %s %d", time.Now().UTC().Format(time.RFC3339), clientIP(r.RemoteAddr), r.Method, r.URL.Path, status)
	})
}

func clientIP(remoteAddr string) string {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return ap.Addr().Unmap().String()
}

func accessLogsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AWS_AI_PROXY_ACCESS_LOGS_ENABLED"))) {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}

func openAccessLogger() (*log.Logger, func(), error) {
	path, err := appLogPath("access.log")
	if err != nil {
		return nil, nil, err
	}
	file, err := openLogFile(path)
	if err != nil {
		return nil, nil, err
	}
	return log.New(file, "", 0), func() { _ = file.Close() }, nil
}

func openErrorLogger(stderr io.Writer) (func(), error) {
	path, err := appLogPath("error.log")
	if err != nil {
		return nil, err
	}
	file, err := openLogFile(path)
	if err != nil {
		return nil, err
	}
	log.SetOutput(io.MultiWriter(stderr, file))
	return func() {
		log.SetOutput(stderr)
		_ = file.Close()
	}, nil
}

func appLogPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".aws-ai-proxy", name), nil
}

func openLogFile(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func getText(client *http.Client, url string) (string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	return string(data), err
}

func getProfiles(client *http.Client, url string) (map[string]string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	var profiles []profileConfig
	if err := json.NewDecoder(resp.Body).Decode(&profiles); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(profiles))
	for _, profile := range profiles {
		out[profile.Name] = profile.Region
	}
	return out, nil
}

func localBaseURL(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", err
	}
	if ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

func loadConfig() error {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("WARN: could not resolve home directory for config template: %v", err)
		return nil
	}
	path := filepath.Join(home, ".aws-ai-proxy", "config")
	if err := ensureConfigFile(path); err != nil {
		log.Printf("WARN: could not ensure config template %s: %v", path, err)
	}
	return loadConfigFile(path)
}

func pidFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".aws-ai-proxy", "aws-ai-proxy.pid"), nil
}

func writePIDFile(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func ensureConfigFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("chmod config dir: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read config: %w", err)
		}
		if err := os.WriteFile(path, []byte(configTemplate()), 0o600); err != nil {
			return fmt.Errorf("create config: %w", err)
		}
		return nil
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod config: %w", err)
	}

	existing := configKeys(data)
	var missing strings.Builder
	for _, item := range configDefaults {
		if existing[item.key] {
			continue
		}
		if missing.Len() == 0 && len(data) > 0 && data[len(data)-1] != '\n' {
			missing.WriteString("\n")
		}
		missing.WriteString(item.key)
		missing.WriteString("=")
		missing.WriteString(item.value)
		missing.WriteString("\n")
	}
	if missing.Len() == 0 {
		return nil
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open config for append: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(missing.String()); err != nil {
		return fmt.Errorf("append config defaults: %w", err)
	}
	return nil
}

func configTemplate() string {
	var b strings.Builder
	b.WriteString("# aws-ai-proxy configuration - environment variables override values set here.\n")
	for _, item := range configDefaults {
		b.WriteString(item.key)
		b.WriteString("=")
		b.WriteString(item.value)
		b.WriteString("\n")
	}
	return b.String()
}

func configKeys(data []byte) map[string]bool {
	keys := map[string]bool{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		keys[strings.TrimSpace(key)] = true
	}
	return keys
}

func loadConfigFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config %s: %w", path, err)
	}

	for lineNo, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("parse config %s:%d: expected KEY=VALUE", path, lineNo+1)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if key == "" {
			return fmt.Errorf("parse config %s:%d: empty key", path, lineNo+1)
		}
		if val == "" {
			if emptyConfigValueAllowed(key) {
				continue
			}
			return fmt.Errorf("parse config %s:%d: empty value for %q", path, lineNo+1, key)
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, val); err != nil {
				return fmt.Errorf("set config %s:%d: %w", path, lineNo+1, err)
			}
		}
	}
	return nil
}

func emptyConfigValueAllowed(key string) bool {
	switch key {
	case "AWS_AI_PROXY_PROFILES", envAWSCLIBinaryPath, envAWSConfigPath:
		return true
	default:
		return false
	}
}

func parseAllowedProfiles(profilesRaw string) map[string]string {
	allowed := make(map[string]string)
	for _, entry := range strings.Split(profilesRaw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, region, _ := strings.Cut(entry, ":")
		name = strings.TrimSpace(name)
		region = strings.TrimSpace(region)
		if name == "" {
			continue
		}
		allowed[name] = region
	}
	return allowed
}

// resolveBindAddr reads AWS_AI_PROXY_BIND and validates it as a host:port pair
// where host is an IP literal. Empty / unset -> defaultBindAddr.
func resolveBindAddr() (string, error) {
	raw := os.Getenv("AWS_AI_PROXY_BIND")
	if raw == "" {
		raw = defaultBindAddr
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return "", fmt.Errorf("AWS_AI_PROXY_BIND=%q: %w", raw, err)
	}
	if host == "" {
		return "", fmt.Errorf("AWS_AI_PROXY_BIND=%q: empty host; use 0.0.0.0:%s to bind all interfaces", raw, port)
	}
	if _, err := netip.ParseAddr(host); err != nil {
		return "", fmt.Errorf("AWS_AI_PROXY_BIND=%q: host must be an IP literal (got %q): %w", raw, host, err)
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return "", fmt.Errorf("AWS_AI_PROXY_BIND=%q: invalid port %q: %w", raw, port, err)
	}
	return raw, nil
}

// resolveAllow parses AWS_AI_PROXY_ALLOW into CIDR prefixes. Bare IPs are
// normalized to /32 (IPv4) or /128 (IPv6).
func resolveAllow() ([]netip.Prefix, error) {
	raw := os.Getenv("AWS_AI_PROXY_ALLOW")
	if raw == "" {
		raw = defaultAllowList
	}
	parts := strings.Split(raw, ",")
	prefixes := make([]netip.Prefix, 0, len(parts))
	seen := map[netip.Prefix]struct{}{}
	for _, p := range parts {
		entry := strings.TrimSpace(p)
		if entry == "" {
			return nil, fmt.Errorf("AWS_AI_PROXY_ALLOW=%q: empty entry - drop the stray comma", raw)
		}
		var pref netip.Prefix
		if strings.Contains(entry, "/") {
			parsed, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, fmt.Errorf("AWS_AI_PROXY_ALLOW: %q: %w", entry, err)
			}
			pref = parsed.Masked()
		} else {
			addr, err := netip.ParseAddr(entry)
			if err != nil {
				return nil, fmt.Errorf("AWS_AI_PROXY_ALLOW: %q is not an IP or CIDR: %w", entry, err)
			}
			addr = addr.Unmap()
			bits := 32
			if addr.Is6() {
				bits = 128
			}
			pref = netip.PrefixFrom(addr, bits)
		}
		if _, dup := seen[pref]; dup {
			continue
		}
		seen[pref] = struct{}{}
		prefixes = append(prefixes, pref)
	}
	return prefixes, nil
}

func allowMiddleware(next http.Handler, allow []netip.Prefix) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ap, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil {
			log.Printf("WARN: blocked request with unparseable RemoteAddr %q: %v", r.RemoteAddr, err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ip := ap.Addr().Unmap()
		for _, p := range allow {
			if p.Contains(ip) {
				next.ServeHTTP(w, r)
				return
			}
		}
		log.Printf("WARN: blocked %s - not in AWS_AI_PROXY_ALLOW", ip)
		http.Error(w, "forbidden", http.StatusForbidden)
	})
}

func newServer(allowed map[string]string, exporter credentialExporter, lookup regionLookup) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /credentials/{profile}", func(w http.ResponseWriter, r *http.Request) {
		profile := r.PathValue("profile")
		if _, ok := allowed[profile]; !ok {
			http.Error(w, "profile not allowed", http.StatusForbidden)
			log.Printf("DENIED: %q", profile)
			return
		}

		out, err := exporter(profile)
		if err != nil {
			http.Error(w, "credential export failed: "+err.Error(), http.StatusBadGateway)
			log.Printf("ERROR: %q: %v", profile, err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
		log.Printf("OK: %q", profile)
	})

	mux.HandleFunc("GET /profiles", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(profileList(allowed, lookup)); err != nil {
			log.Printf("ERROR: encode profiles: %v", err)
		}
	})

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(version + "\n"))
	})

	return mux
}

func profileNames(allowed map[string]string) []string {
	profiles := make([]string, 0, len(allowed))
	for p := range allowed {
		profiles = append(profiles, p)
	}
	sort.Strings(profiles)
	return profiles
}

func profileList(allowed map[string]string, lookup regionLookup) []profileConfig {
	names := profileNames(allowed)
	profiles := make([]profileConfig, 0, len(names))
	for _, name := range names {
		region := allowed[name]
		if region == "" && lookup != nil {
			region = lookup(name)
		}
		profiles = append(profiles, profileConfig{Name: name, Region: region})
	}
	return profiles
}

func lookupRegion(profile string) string {
	awsPath, err := resolveAWSCLIPath()
	if err != nil {
		log.Printf("WARN: region lookup for %q skipped: %v", profile, err)
		return ""
	}
	cmd := exec.Command(awsPath, "configure", "get", "region", "--profile", profile)
	withAWSConfigFile(cmd)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
			log.Printf("WARN: region lookup for %q failed: %s", profile, strings.TrimSpace(string(exitErr.Stderr)))
		} else {
			log.Printf("WARN: region lookup for %q failed: %v", profile, err)
		}
		return ""
	}
	return strings.TrimSpace(string(out))
}

func exportCredentials(profile string) ([]byte, error) {
	awsPath, err := resolveAWSCLIPath()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(awsPath, "configure", "export-credentials", "--profile", profile)
	withAWSConfigFile(cmd)
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && len(exitErr.Stderr) > 0 {
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(exitErr.Stderr)))
	}
	return nil, err
}

func resolveAWSCLIPath() (string, error) {
	configured := strings.TrimSpace(os.Getenv(envAWSCLIBinaryPath))
	if configured != "" {
		configured = expandHome(configured)
		if err := executableFile(configured); err != nil {
			return "", fmt.Errorf("%s=%q is not executable: %w", envAWSCLIBinaryPath, configured, err)
		}
		return configured, nil
	}

	attempted := []string{"PATH:aws"}
	if path, err := exec.LookPath("aws"); err == nil {
		return path, nil
	}
	for _, candidate := range commonAWSCLIPaths {
		attempted = append(attempted, candidate)
		if executableFile(candidate) == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("aws CLI not found (attempted %s); set %s", strings.Join(attempted, ", "), envAWSCLIBinaryPath)
}

func executableFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("is a directory")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("permission denied")
	}
	return nil
}

func withAWSConfigFile(cmd *exec.Cmd) {
	if path := awsConfigFile(); path != "" {
		cmd.Env = append(os.Environ(), "AWS_CONFIG_FILE="+path)
	}
}

func awsConfigFile() string {
	raw := strings.TrimSpace(os.Getenv(envAWSConfigPath))
	if raw == "" {
		return ""
	}
	path := expandHome(raw)
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return filepath.Join(path, "config")
	}
	return path
}

func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/"))
}
