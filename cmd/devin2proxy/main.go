// Command devin2proxy serves an OpenAI-compatible HTTP API backed by the Devin
// CLI's stored credential.
package main

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"devin2proxy/internal/dashboard"
	"devin2proxy/internal/devin"
	"devin2proxy/internal/eventlog"
	"devin2proxy/internal/openai"
	"devin2proxy/internal/server"
)

// Config is the on-disk configuration. It is written on first run with a
// generated API key so the server can never start unauthenticated by accident.
type Config struct {
	Addr   string `json:"addr"`
	APIKey string `json:"api_key"`
	// TLS terminates HTTPS in this process. For a public deployment with no
	// domain — a bare IP behind a reverse-proxy-less Portainer — there is no CA
	// that will issue a certificate, so the proxy ships its own: with TLS on and
	// no cert/key named, a self-signed pair is generated beside the config on
	// first start and its fingerprint is printed for the operator to check once.
	TLS bool `json:"tls"`
	// TLSCertFile and TLSKeyFile override the generated pair, for an operator
	// who later gets a domain and a real certificate. Naming either one implies
	// the directory of the other default is not wanted.
	TLSCertFile string `json:"tls_cert_file,omitempty"`
	TLSKeyFile  string `json:"tls_key_file,omitempty"`
	// TLSNames adds subject-alt names — an IP or a DNS name, comma separated —
	// to the generated certificate. The public IP of the machine belongs here:
	// without it clients report a name mismatch on top of the trust warning,
	// and a mismatch no bypass click can always clear.
	TLSNames        string   `json:"tls_names,omitempty"`
	Model           string   `json:"model"`
	Models          []string `json:"models"`
	AllowOrigins    string   `json:"allow_origins"`
	MaxConcurrent   int      `json:"max_concurrent"`
	RequestTimeoutS int      `json:"request_timeout_seconds"`
	// MinMaxTokens is a floor on a client's max_tokens. The backend spends the
	// budget on the model's reasoning before it emits any answer, so a small
	// request (autocomplete clients ask for 64-256) returns nothing without it.
	MinMaxTokens int `json:"min_max_tokens"`
	// Tokens is a list of account credentials to rotate through, one per request,
	// so several free-tier accounts can share the load. Each entry is a full
	// `devin-session-token$<jwt>` value, exactly as it appears in the CLI's
	// credentials.toml. When this (or TokensFile) is set, the CLI's own stored
	// credential is not used at all.
	Tokens []string `json:"tokens"`
	// TokensFile points at a file holding one token per line; blank lines and
	// `#` comments are ignored. Prefer this over Tokens for anything but a quick
	// test, since it keeps credentials out of the config file.
	TokensFile string `json:"tokens_file"`
	// Proxies is a list of outbound routes to rotate through, one per request, in
	// the same way accounts rotate. Each entry is a proxy URL — socks5://,
	// socks5h://, http:// or https://, optionally with user:pass@ — or the word
	// direct to include the machine's own connection in the rotation. It exists
	// for address diversity; see docs/PROTOCOL.md.
	Proxies []string `json:"proxies"`
	// ProxiesFile points at a file holding one proxy URL per line, with the same
	// blank-line and `#` handling as TokensFile.
	ProxiesFile string `json:"proxies_file"`
	// QuotaCooldown, on by default, lets the pool ask the backend how much quota a
	// rate-limited account has left and hold it out until that quota resets,
	// instead of retrying a spent account every 30 seconds. The answer can only
	// lengthen a cooldown the backend already earned, never create one; set this
	// to false to keep every cooldown purely a reaction to a refusal.
	QuotaCooldown *bool `json:"quota_cooldown"`

	// Dashboard, on by default, serves the operator's control panel: the account
	// pool, the outbound routes and a live view of requests. It is a second HTTP
	// surface with its own password, deliberately separate from the API key on
	// /v1 — that key is pasteable into a client, this one is not.
	Dashboard *bool `json:"dashboard"`
	// DashboardPassword is the password for that panel. Leaving it empty is the
	// better choice: the dashboard then keeps a hash of a password generated on
	// first run, prints it once, and never stores the password itself. Set it here
	// (0600 file, gitignored) or through DEVIN2PROXY_DASHBOARD_PASSWORD to choose
	// your own; a change replaces the stored hash.
	DashboardPassword string `json:"dashboard_password,omitempty"`
	// DashboardAllowRemote permits the dashboard from outside this machine. Off by
	// default: it can add and delete credentials.
	DashboardAllowRemote bool `json:"dashboard_allow_remote,omitempty"`
	// DevinCLI is the Devin CLI on this machine. The dashboard's sign-in does not run
	// it — the exchange is done in-process — but the path is shown in the command a
	// person can run in a terminal, and the sign-out endpoint runs it to clear the
	// credential the CLI holds. Empty means the usual locations are searched.
	DevinCLI string `json:"devin_cli,omitempty"`
	// DashboardWebappHost is where the dashboard's sign-in page lives, for a
	// deployment other than app.devin.ai. It must be the host that serves
	// /auth/cli/continue; the default is the one the CLI itself uses.
	DashboardWebappHost string `json:"dashboard_webapp_host,omitempty"`
	// DashboardEchoURL is fetched through a route when one is tested, to report the
	// address it exits from. It must answer with the caller's address as plain
	// text. Set it to "-" to skip that second request.
	DashboardEchoURL string `json:"dashboard_echo_url,omitempty"`
}

// defaultModels are the identifiers clients may ask for. Free-tier accounts
// only serve swe-1.6, and the backend accepts exactly one uid for it
// (swe-1-6-slow); the rest are aliases this proxy resolves to that uid.
var defaultModels = []string{"swe-1-6-slow", "swe-1.6", "swe"}

func main() {
	configPath := flag.String("config", "config.json", "path to the JSON config file")
	addr := flag.String("addr", "", "listen address (overrides config)")
	printKey := flag.Bool("print-key", false, "print the configured API key and exit")
	dashboardOn := flag.Bool("dashboard", true, "serve the operator dashboard at /dashboard")
	dashboardPassword := flag.String("dashboard-password", "", "dashboard password (overrides config; stored as a hash)")
	dashboardRemote := flag.Bool("dashboard-allow-remote", false, "allow the dashboard from outside this machine")
	tlsFlag := flag.Bool("tls", false, "serve HTTPS (generates a self-signed certificate beside the config when none is named)")
	tlsCertFlag := flag.String("tls-cert", "", "TLS certificate file (default: tls.crt beside the config)")
	tlsKeyFlag := flag.String("tls-key", "", "TLS private key file (default: tls.key beside the config)")
	tlsNamesFlag := flag.String("tls-names", "", "extra certificate subject names, comma separated (the machine's public IP belongs here)")
	flag.Parse()

	// A bare relative default is read against the working directory, which would
	// give `bin\devin2proxy.exe` a different config — and so a different API key —
	// depending on whether it was launched from the project root or from inside
	// bin\. Resolve it instead, but only when the user did not name a path.
	configExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			configExplicit = true
		}
	})
	if !configExplicit {
		*configPath = resolveConfigPath(*configPath)
	}

	cfg, created, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *printKey {
		fmt.Println(cfg.APIKey)
		return
	}
	applyEnv(&cfg)
	if *addr != "" {
		cfg.Addr = *addr
	}

	// The event hub is installed before anything else logs, so the dashboard's log
	// view has the startup lines in it — which is where the answer to "why did it
	// fail to start" usually is.
	events := eventlog.New(eventlog.DefaultCapacity)
	log.SetOutput(io.MultiWriter(os.Stderr, events))

	store, err := dashboard.OpenStore(dashboardStatePath(*configPath))
	if err != nil {
		log.Fatalf("dashboard state: %v", err)
	}

	// The pool is always non-nil, so an account added or removed through the
	// dashboard takes effect on the next request without a restart. An empty pool
	// means the CLI's own stored credential is used, which is a supported way to run
	// rather than a misconfiguration.
	pool, accountsExternal := buildAccountPool(cfg, store)
	if pool.Len() > 0 {
		log.Printf("account pool: %d token(s) loaded from %s, rotating one per request",
			pool.Len(), sourceLabel(accountsExternal, "the dashboard's own list"))
	} else {
		creds, err := devin.LoadCredentials()
		if err != nil {
			log.Printf("WARNING: no Devin credential available yet: %v", err)
			log.Printf("         run `devin auth login`, then start requests; the credential is re-read per request")
		} else {
			log.Printf("no accounts in the pool; using the credential from %s (backend %s)",
				creds.Source, creds.APIServerURL)
		}
	}

	if created {
		log.Printf("wrote %s with a freshly generated API key", *configPath)
	}
	if cfg.APIKey == "" {
		log.Fatalf("no API key configured; set DEVIN2PROXY_API_KEY or delete %s to regenerate", *configPath)
	}

	// Routes are validated up front too: a typo in a proxy URL is a start-up
	// error, not something to discover on the first completion.
	egress, proxiesExternal, err := buildRoutePool(cfg, store)
	if err != nil {
		log.Fatalf("proxies: %v", err)
	}
	if egress != nil {
		log.Printf("outbound routes: %d loaded from %s, rotating one per request",
			egress.Len(), sourceLabel(proxiesExternal, "the dashboard's own list"))
	}

	client := devin.NewClient(devin.Options{
		MaxConcurrent: cfg.MaxConcurrent,
		HeaderTimeout: 60 * time.Second,
		Egress:        egress,
		// With more than one account, a 429 is answered by trying the next account
		// rather than by waiting out a backoff against the one that just refused. A
		// single account has no alternative to offer, so it keeps the full budget.
		RateLimitRetries: rateLimitRetries(pool),
	})

	// Quota-aware cooldowns need the client to reach the status endpoint, so this
	// is wired after it exists. The fetch only ever runs after the backend has
	// refused a request, which is why a failure to reach it changes nothing.
	if cfg.QuotaCooldown == nil || *cfg.QuotaCooldown {
		pool.SetStatusSource(client.FetchAccountStatus)
		log.Printf("quota-aware cooldown: on; a rate-limited account is held out until its quota resets")
	} else {
		log.Printf("quota-aware cooldown: off; accounts cool only in reaction to a refusal")
	}

	var panel http.Handler
	if dashboardWanted(*dashboardOn, cfg) {
		panel, err = buildDashboard(dashboardOptions{
			cfg:          cfg,
			store:        store,
			events:       events,
			pool:         pool,
			client:       client,
			password:     firstNonEmpty(*dashboardPassword, os.Getenv("DEVIN2PROXY_DASHBOARD_PASSWORD"), cfg.DashboardPassword),
			allowRemote:  *dashboardRemote || cfg.DashboardAllowRemote,
			accountsFrom: accountsExternal,
			proxiesFrom:  proxiesExternal,
		})
		if err != nil {
			log.Fatalf("dashboard: %v", err)
		}
	}

	// The default model is the one thing sent to the backend verbatim: aliases in
	// the advertised list are resolved, but an unknown default becomes
	// chat_model_uid as typed, and the backend answers it with an opaque
	// failed_precondition on every single request. A typo here is a start-up
	// error, the same as a typo in a proxy URL.
	if !server.IsBackendModelUID(cfg.Model) {
		log.Fatalf("config: model %q is not a backend model; the backend accepts %s",
			cfg.Model, strings.Join(server.ForwardedModelUIDs(), ", "))
	}

	srv := server.New(server.Config{
		APIKey:         cfg.APIKey,
		DefaultModel:   cfg.Model,
		Models:         cfg.Models,
		AllowOrigins:   cfg.AllowOrigins,
		RequestTimeout: time.Duration(cfg.RequestTimeoutS) * time.Second,
		MinMaxTokens:   cfg.MinMaxTokens,
		Creds:          pool,
		Events:         events,
		Dashboard:      panel,
	}, client)

	// TLS resolves before anything prints, so a certificate problem is a
	// start-up error like any other misconfiguration.
	useTLS := cfg.TLS || *tlsFlag
	var tlsCert tls.Certificate
	certPath, keyPath := "", ""
	if useTLS {
		certDir := filepath.Dir(*configPath)
		if abs, err := filepath.Abs(certDir); err == nil {
			certDir = abs // the log lines name the file the operator must actually find
		}
		certPath = firstNonEmpty(*tlsCertFlag, cfg.TLSCertFile, filepath.Join(certDir, "tls.crt"))
		keyPath = firstNonEmpty(*tlsKeyFlag, cfg.TLSKeyFile, filepath.Join(certDir, "tls.key"))
		names := splitList(firstNonEmpty(*tlsNamesFlag, cfg.TLSNames))
		var generated bool
		var err error
		tlsCert, generated, err = serveCertificate(certPath, keyPath, names, true)
		if err != nil {
			log.Fatalf("tls: %v", err)
		}
		warnCertExpiry(tlsCert, certPath)
		if generated {
			log.Printf("tls: generated a self-signed certificate at %s", certPath)
		}
		log.Printf("tls: SHA-256 fingerprint %s", certFingerprint(tlsCert))
		log.Printf("tls: give clients this file to trust: %s", certPath)
	}

	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	log.Printf("listening on %s://%s", scheme, cfg.Addr)
	log.Printf("default model: %s", cfg.Model)
	log.Printf("models: %s", strings.Join(cfg.Models, ", "))
	log.Printf("api key: %s…%s (use `-print-key` to show it in full)", prefix(cfg.APIKey, 6), suffix(cfg.APIKey, 4))
	log.Printf("point an OpenAI client at %s://%s/v1 with that key", scheme, cfg.Addr)
	if panel != nil {
		log.Printf("dashboard: %s://%s/dashboard/", scheme, cfg.Addr)
	}
	warnIfExposed(cfg, useTLS)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv,
		ReadHeaderTimeout: 30 * time.Second,
		// No WriteTimeout: completions legitimately stream for minutes.
	}
	if useTLS {
		httpSrv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{tlsCert},
			MinVersion:   tls.VersionTLS12,
		}
		// The certificate is already loaded; naming files here would read a
		// second copy from disk.
		if err := httpSrv.ListenAndServeTLS("", ""); err != nil {
			log.Fatalf("serve: %v", err)
		}
		return
	}
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// warnIfExposed says plainly what binding to a non-loopback address means. It is
// a warning rather than a refusal because serving a LAN is a legitimate setup, but
// it is the difference between a private proxy and an open invitation to spend the
// account's quota, and that deserves a line in the log rather than a footnote.
func warnIfExposed(cfg Config, tlsOn bool) {
	if isLoopbackListen(cfg.Addr) {
		return
	}
	log.Printf("WARNING: listening on %s, which is reachable from outside this machine.", cfg.Addr)
	log.Printf("         Anyone who can reach it and knows the API key can spend your Devin quota.")
	if !tlsOn {
		log.Printf("         WITHOUT TLS the key and the dashboard password cross the wire in" +
			" cleartext; set DEVIN2PROXY_TLS=1 to serve HTTPS with a generated certificate.")
	}
	if !cfg.DashboardAllowRemote {
		log.Printf("         The dashboard is still refused to non-local callers; pass" +
			" -dashboard-allow-remote to change that.")
	}
}

// isLoopbackListen reports whether a listen address is this machine only. An
// address with no host part (":8788") is every interface, which is not loopback.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// dashboardWanted reports whether the panel should be served: the flag decides
// unless the config file says otherwise.
func dashboardWanted(flagValue bool, cfg Config) bool {
	if cfg.Dashboard != nil {
		return *cfg.Dashboard
	}
	return flagValue
}

// dashboardStatePath puts the dashboard's own file beside the config, so both move
// together and both stay out of the repository.
func dashboardStatePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "dashboard.json")
}

// sourceLabel turns an "external source" note into something a log line can read.
func sourceLabel(external, fallback string) string {
	if external == "" {
		return fallback
	}
	return external
}

// resolveConfigPath prefers a config in the working directory, which is where a
// user editing one by hand would naturally keep it, and otherwise uses the one
// beside the executable — so `bin\devin2proxy.exe` finds `bin\config.json` however
// it was launched. Nothing existing is ever overwritten by the fallback; it only
// decides which file is read, and a missing file is still created in the working
// directory as before.
func resolveConfigPath(path string) string {
	if _, err := os.Stat(path); err == nil {
		return path
	}
	exe, err := os.Executable()
	if err != nil {
		return path
	}
	beside := filepath.Join(filepath.Dir(exe), filepath.Base(path))
	if _, err := os.Stat(beside); err == nil {
		return beside
	}
	return path
}

func loadConfig(path string) (Config, bool, error) {
	cfg := Config{
		Addr:            "127.0.0.1:8788",
		Model:           devin.DefaultModel,
		Models:          defaultModels,
		AllowOrigins:    "*",
		MaxConcurrent:   2,
		RequestTimeoutS: 600,
		MinMaxTokens:    openai.DefaultMinMaxTokens,
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, false, fmt.Errorf("parse %s: %w", path, err)
		}
		if len(cfg.Models) == 0 {
			cfg.Models = defaultModels
		}
		if cfg.Model == "" {
			cfg.Model = devin.DefaultModel
		}
		if cfg.Addr == "" {
			cfg.Addr = "127.0.0.1:8788"
		}
		if cfg.MaxConcurrent <= 0 {
			cfg.MaxConcurrent = 2
		}
		if cfg.RequestTimeoutS <= 0 {
			cfg.RequestTimeoutS = 600
		}
		if cfg.MinMaxTokens <= 0 {
			cfg.MinMaxTokens = openai.DefaultMinMaxTokens
		}
		if cfg.APIKey == "" && !envSuppliesAPIKey() {
			key, err := generateKey()
			if err != nil {
				return cfg, false, err
			}
			cfg.APIKey = key
			return cfg, true, writeConfig(path, cfg)
		}
		// Either the file carried a key, or DEVIN2PROXY_API_KEY will supply it
		// (applyEnv runs next). Neither case has anything to persist, so the
		// file is left untouched — which is what lets a container run with a
		// read-only root filesystem.
		return cfg, false, nil

	case os.IsNotExist(err):
		// A missing file is the normal first run on a desktop: generate a key
		// and write it so the operator has something to point a client at. In
		// an env-driven deployment the key comes from DEVIN2PROXY_API_KEY, so
		// there is nothing to generate and nothing to write — writing would
		// also demand a writable filesystem a container may not have.
		if envSuppliesAPIKey() {
			return cfg, false, nil
		}
		key, err := generateKey()
		if err != nil {
			return cfg, false, err
		}
		cfg.APIKey = key
		return cfg, true, writeConfig(path, cfg)

	default:
		return cfg, false, err
	}
}

// envSuppliesAPIKey reports whether the API key is coming from the environment.
// It is the difference between "first run on a desktop, persist a generated key"
// and "container deployment, everything is an env var": in the latter the config
// file is neither needed nor written.
func envSuppliesAPIKey() bool {
	return strings.TrimSpace(os.Getenv("DEVIN2PROXY_API_KEY")) != ""
}

func writeConfig(path string, cfg Config) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// 0600: the file holds the key that authorises quota spend.
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func applyEnv(cfg *Config) {
	env := func(name string) (string, bool) {
		v, ok := os.LookupEnv(name)
		if !ok || strings.TrimSpace(v) == "" {
			return "", false
		}
		return strings.TrimSpace(v), true
	}
	if v, ok := env("DEVIN2PROXY_API_KEY"); ok {
		cfg.APIKey = v
	}
	if v, ok := env("DEVIN2PROXY_ADDR"); ok {
		cfg.Addr = v
	}
	if v, ok := env("DEVIN2PROXY_MODEL"); ok {
		cfg.Model = v
	}
	if v, ok := env("DEVIN2PROXY_MODELS"); ok {
		parts := strings.Split(v, ",")
		// A fresh slice, not cfg.Models[:0]: when the config file did not name
		// models, cfg.Models IS the defaultModels global, and reusing its
		// backing array would rewrite the package-level defaults for anything
		// else in the process that reads them.
		models := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				models = append(models, p)
			}
		}
		cfg.Models = models
	}
	if v, ok := env("DEVIN2PROXY_ALLOW_ORIGINS"); ok {
		cfg.AllowOrigins = v
	}
	if v, ok := env("DEVIN2PROXY_MAX_CONCURRENT"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxConcurrent = n
		}
	}
	if v, ok := env("DEVIN2PROXY_MIN_MAX_TOKENS"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MinMaxTokens = n
		}
	}
	if v, ok := env("DEVIN2PROXY_TOKENS"); ok {
		// Comma separated, so a pool can be tried without writing a file.
		cfg.Tokens = splitList(v)
	} else if v, ok := env("DEVIN2PROXY_TOKENS_FILE"); ok {
		cfg.TokensFile = v
	}
	if v, ok := env("DEVIN2PROXY_PROXIES"); ok {
		// Comma separated, matching DEVIN2PROXY_TOKENS.
		cfg.Proxies = splitList(v)
	} else if v, ok := env("DEVIN2PROXY_PROXIES_FILE"); ok {
		cfg.ProxiesFile = v
	}
	if v, ok := env("DEVIN2PROXY_QUOTA_COOLDOWN"); ok {
		on := parseBool(v)
		cfg.QuotaCooldown = &on
	}
	if v, ok := env("DEVIN2PROXY_DASHBOARD"); ok {
		on := parseBool(v)
		cfg.Dashboard = &on
	}
	if v, ok := env("DEVIN2PROXY_DASHBOARD_PASSWORD"); ok {
		cfg.DashboardPassword = v
	}
	if v, ok := env("DEVIN2PROXY_DASHBOARD_ALLOW_REMOTE"); ok {
		cfg.DashboardAllowRemote = parseBool(v)
	}
	if v, ok := env("DEVIN2PROXY_DASHBOARD_ECHO_URL"); ok {
		cfg.DashboardEchoURL = v
	}
	if v, ok := env("DEVIN2PROXY_TLS"); ok {
		cfg.TLS = parseBool(v)
	}
	if v, ok := env("DEVIN2PROXY_TLS_CERT_FILE"); ok {
		cfg.TLSCertFile = v
	}
	if v, ok := env("DEVIN2PROXY_TLS_KEY_FILE"); ok {
		cfg.TLSKeyFile = v
	}
	if v, ok := env("DEVIN2PROXY_TLS_NAMES"); ok {
		cfg.TLSNames = v
	}
	if v, ok := env("DEVIN2PROXY_DEVIN_CLI"); ok {
		cfg.DevinCLI = v
	}
	if v, ok := env("DEVIN2PROXY_DASHBOARD_WEBAPP_HOST"); ok {
		cfg.DashboardWebappHost = v
	}
}

// parseBool reads the handful of spellings an operator might reasonably use for
// an on/off environment variable.
func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on", "enabled":
		return true
	default:
		return false
	}
}

// rateLimitRetries decides how many times one request may be retried on the same
// account after a 429. It is one — no same-account retry — only when the pool can
// offer another account, because a free-tier allowance does not refill within the
// sub-second backoff that a retry costs.
func rateLimitRetries(pool *devin.Pool) int {
	if pool.Len() > 1 {
		return 1
	}
	return 0
}

// splitList splits a comma- or newline-separated list, dropping empty entries.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// buildAccountPool assembles the account pool and reports where its contents came
// from. The pool is always non-nil, because the dashboard edits it in place: a
// shared value that starts empty and grows is what makes an account added from the
// panel take effect on the very next request.
//
// Precedence is env/config over the dashboard's own list, and the note returned in
// the second value is what the dashboard shows to explain a list it cannot edit.
// Handing the dashboard a pool it cannot change is deliberate: a list maintained in
// config.json should not be silently editable from a browser.
func buildAccountPool(cfg Config, store *dashboard.Store) (*devin.Pool, string) {
	apiURL := firstNonEmpty(os.Getenv("WINDSURF_API_SERVER_URL"), os.Getenv("DEVIN_API_SERVER_URL"))

	tokens := cfg.Tokens
	if cfg.TokensFile != "" {
		fromFile, err := devin.TokensFromFile(cfg.TokensFile)
		if err != nil {
			log.Fatalf("tokens_file: %v", err)
		}
		tokens = append(tokens, fromFile...)
	}
	if len(tokens) > 0 {
		return devin.NewPool(tokens, apiURL), "config.json (tokens or tokens_file)"
	}
	return devin.NewPool(store.Accounts(), apiURL), ""
}

// buildRoutePool assembles the outbound route pool, with the same precedence and
// the same reason for it: the pool is created once and the dashboard swaps its
// contents, so a route change does not need a restart.
func buildRoutePool(cfg Config, store *dashboard.Store) (*devin.EgressPool, string, error) {
	specs := cfg.Proxies
	if cfg.ProxiesFile != "" {
		fromFile, err := devin.RoutesFromFile(cfg.ProxiesFile)
		if err != nil {
			return nil, "", fmt.Errorf("proxies_file: %w", err)
		}
		specs = append(specs, fromFile...)
	}
	if len(specs) > 0 {
		pool, err := devin.NewEgressPool(specs, egressOptions())
		return pool, "config.json (proxies or proxies_file)", err
	}
	pool, err := devin.NewEgressPool(store.Proxies(), egressOptions())
	return pool, "", err
}

func egressOptions() devin.EgressOptions {
	return devin.EgressOptions{HeaderTimeout: 60 * time.Second, DialTimeout: 20 * time.Second}
}

// dashboardOptions is everything buildDashboard needs, gathered so the call site
// stays readable.
type dashboardOptions struct {
	cfg          Config
	store        *dashboard.Store
	events       *eventlog.Hub
	pool         *devin.Pool
	client       *devin.Client
	password     string
	allowRemote  bool
	accountsFrom string
	proxiesFrom  string
}

// buildDashboard sets the password and constructs the panel.
//
// With no password configured, one is generated and printed exactly once, and only
// its hash is kept. The alternative — refusing to start the dashboard until a
// password is set — would mean the feature is invisible to whoever just installed
// it, and a password that is regenerated on every restart would be worse still.
func buildDashboard(o dashboardOptions) (http.Handler, error) {
	switch {
	case o.password != "":
		if err := o.store.SetPasswordText(o.password); err != nil {
			return nil, err
		}
		log.Printf("dashboard password set from the configuration")
	case o.store.Password() == nil:
		password, err := o.store.GeneratePassword()
		if err != nil {
			return nil, err
		}
		// The generated password is the one secret that must never pass through
		// the standard logger: main tees it into the event hub, and the hub feeds
		// the dashboard's log view — so a logged password would stay retrievable
		// from /dashboard/api/logs/recent long after startup, and survive a
		// logout's session-epoch bump. It goes straight to stderr instead.
		fmt.Fprintf(os.Stderr, "dashboard: no password was configured, so one was generated for you:\n")
		fmt.Fprintf(os.Stderr, "            %s\n", password)
		fmt.Fprintf(os.Stderr, "           write it down now; only its hash is stored, so it cannot be shown again.\n")
		fmt.Fprintf(os.Stderr, "           choose your own with -dashboard-password or dashboard_password in config.json\n")
	}

	echoURL := firstNonEmpty(o.cfg.DashboardEchoURL, defaultEchoURL)
	if echoURL == "-" {
		echoURL = ""
	}
	remote := o.allowRemote
	if remote {
		log.Printf("WARNING: the dashboard is reachable from outside this machine and can add and delete credentials")
	}
	return dashboard.New(dashboard.Options{
		Pool:     o.pool,
		Client:   o.client,
		Events:   o.events,
		Store:    o.store,
		DevinCLI: firstNonEmpty(o.cfg.DevinCLI, defaultDevinCLI()),
		// These arguments are only ever shown to the operator as the command to run
		// in a terminal. Adding an account from the dashboard does not run the CLI:
		// the page fetches a sign-in URL, and the code that comes back is exchanged by
		// this process. The flag is still the right thing to print, because it is the
		// command that works for a person who would rather use the CLI's own flow.
		LoginArgs: []string{"auth", "login", "--force-manual-token-flow"},
		// The sign-out is a CLI call, and the only one left: it clears the credential
		// the CLI holds so a different account can be used there. The account is put
		// into the pool first, so nothing is lost by it.
		LogoutArgs: []string{"auth", "logout"},
		// Where the sign-in page lives. The default is the host the CLI's own manual
		// flow uses, which is the one that serves /auth/cli/continue.
		WebappHost: firstNonEmpty(o.cfg.DashboardWebappHost, devin.DefaultWebappHost),
		// A sign-in exchanges its code against the same backend requests go to, so a
		// credential added here is one the pool can serve with.
		APIServerURL:     backendURL(),
		EchoURL:          echoURL,
		ProbeTarget:      probeTarget(backendURL()),
		Egress:           egressOptions(),
		AllowRemote:      remote,
		AccountsExternal: o.accountsFrom,
		ProxiesExternal:  o.proxiesFrom,
		// The uids the server passes through when a client names them. The Models panel
		// shows each one with what the catalogue says about it, so a model the account's
		// plan refuses is visible there rather than only in a client's failed request.
		Forwarded: server.ForwardedModelUIDs(),
		Info: dashboard.Info{
			Addr:          o.cfg.Addr,
			Backend:       backendURL(),
			Model:         o.cfg.Model,
			APIKey:        prefix(o.cfg.APIKey, 10) + "…" + suffix(o.cfg.APIKey, 4),
			Models:        o.cfg.Models,
			QuotaCooldown: o.cfg.QuotaCooldown == nil || *o.cfg.QuotaCooldown,
		},
	}), nil
}

// defaultEchoURL is fetched through a route when the operator tests it, to report
// the address the route exits from. It is a plain-text echo service and is only
// contacted when a test is asked for.
const defaultEchoURL = "https://api.ipify.org"

// backendURL is the inference backend the pool's accounts were built against,
// falling back to whatever the CLI is configured for.
func backendURL() string {
	if url := firstNonEmpty(os.Getenv("WINDSURF_API_SERVER_URL"), os.Getenv("DEVIN_API_SERVER_URL")); url != "" {
		return url
	}
	if creds, err := devin.LoadCredentials(); err == nil {
		return creds.APIServerURL
	}
	return ""
}

// probeTarget turns that backend URL into the address a route test dials.
//
// The dialer takes a host:port, not a URL, and handing it a URL fails with "too
// many colons in address" — which is exactly what a route test reported until this
// existed. The port is what the probe needs, since the point of a route test is to
// open a tunnel and negotiate TLS through it.
func probeTarget(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "https" {
		return u.Host + ":443"
	}
	return u.Host + ":80"
}

// defaultDevinCLI finds the Devin CLI. The installer's location is checked as well
// as PATH, because a Windows install does not always add itself to the path of the
// shell the proxy was started from.
func defaultDevinCLI() string {
	if path, err := exec.LookPath("devin"); err == nil {
		return path
	}
	if appData := os.Getenv("LOCALAPPDATA"); appData != "" {
		candidate := filepath.Join(appData, "devin", "cli", "bin", "devin.exe")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// firstNonEmpty returns the first non-empty string, for the precedence chain
// between a flag, the environment, the config file and a stored value.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func generateKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return "sk-devin-" + hex.EncodeToString(b), nil
}

func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func suffix(s string, n int) string {
	if len(s) <= n {
		return ""
	}
	return s[len(s)-n:]
}
