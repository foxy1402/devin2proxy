package devin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Credentials mirrors the fields of the CLI's credentials.toml this proxy uses.
type Credentials struct {
	// APIKey is the bearer value, e.g. "devin-session-token$<jwt>".
	APIKey string
	// APIServerURL is the inference backend, normally https://server.codeium.com.
	APIServerURL string
	// Source names where the credential was read from, for logging only.
	Source string
	// poolIndex is the slot this credential occupies in a Pool, or -1 when it did
	// not come from one. Pool.Report verifies the slot really points back at this
	// credential before acting on it.
	poolIndex int
}

// Label names the account for a log line or a dashboard row: the last few
// characters of the token, which is enough to tell two accounts apart and never
// enough to use one. It is the only form of a credential that is ever displayed.
func (c *Credentials) Label() string {
	if c == nil {
		return ""
	}
	return tail(c.APIKey)
}

// newCredentials returns a credential outside any pool, with the pool slot marked
// as unused.
func newCredentials(apiKey, apiServerURL, source string) *Credentials {
	return &Credentials{
		APIKey:       apiKey,
		APIServerURL: apiServerURL,
		Source:       source,
		poolIndex:    -1,
	}
}

const defaultAPIServerURL = "https://server.codeium.com"

// DefaultAPIServerURL is the inference backend used when nothing overrides it. It
// is exported because the dashboard's sign-in has to exchange its code against the
// same backend the credentials are for, and it should not carry its own copy of
// this address.
func DefaultAPIServerURL() string { return defaultAPIServerURL }

// LoadCredentials resolves the CLI's credential, re-reading the file on every
// call so a re-login through the CLI is picked up without restarting the
// server.
//
// Precedence: DEVIN_TOKEN, then WINDSURF_API_KEY, then the credentials file
// (DEVIN_CREDENTIALS_PATH, then the platform default location).
func LoadCredentials() (*Credentials, error) {
	apiURL := strings.TrimRight(firstNonEmpty(os.Getenv("WINDSURF_API_SERVER_URL"), os.Getenv("DEVIN_API_SERVER_URL")), "/")

	if tok := firstNonEmpty(os.Getenv("DEVIN_TOKEN"), os.Getenv("WINDSURF_API_KEY")); tok != "" {
		if apiURL == "" {
			apiURL = defaultAPIServerURL
		}
		return newCredentials(tok, apiURL, "environment"), nil
	}

	path, err := credentialsPath()
	if err != nil {
		return nil, err
	}
	creds, err := parseCredentialsFile(path)
	if err != nil {
		return nil, err
	}
	creds.Source = path
	// An explicit env override wins over what the file recorded, so a
	// redirected backend can be pointed at without editing the CLI's state.
	if apiURL != "" {
		creds.APIServerURL = apiURL
	}
	if creds.APIServerURL == "" {
		creds.APIServerURL = defaultAPIServerURL
	}
	if creds.APIKey == "" {
		return nil, fmt.Errorf("%s contains no windsurf_api_key; run `devin auth login`", path)
	}
	return creds, nil
}

// credentialsPath returns the location of credentials.toml.
func credentialsPath() (string, error) {
	if p := os.Getenv("DEVIN_CREDENTIALS_PATH"); p != "" {
		return p, nil
	}
	if appData := os.Getenv("APPDATA"); appData != "" {
		return filepath.Join(appData, "devin", "credentials.toml"), nil
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "devin", "credentials.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate credentials: %w", err)
	}
	return filepath.Join(home, ".local", "share", "devin", "credentials.toml"), nil
}

// parseCredentialsFile reads the small TOML file the CLI writes. Only the known
// top-level string keys this proxy uses are interpreted — everything else,
// including keys it once read but no longer needs, is skipped — so this stays a
// few lines rather than pulling in a TOML parser.
func parseCredentialsFile(path string) (*Credentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credentials (%s): %w", path, err)
	}
	out := &Credentials{poolIndex: -1}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = unquoteTOML(strings.TrimSpace(value))

		switch key {
		case "windsurf_api_key":
			out.APIKey = value
		case "api_server_url":
			out.APIServerURL = strings.TrimRight(value, "/")
		}
	}
	return out, nil
}

// unquoteTOML strips surrounding quotes and resolves the escape sequences the
// CLI actually emits (backslash and quote).
func unquoteTOML(s string) string {
	if len(s) < 2 {
		return s
	}
	// Drop a trailing comment only when the value is not a quoted string.
	if s[0] != '"' && s[0] != '\'' {
		if i := strings.IndexByte(s, '#'); i >= 0 {
			return strings.TrimSpace(s[:i])
		}
		return s
	}
	if s[0] == '\'' {
		if end := strings.LastIndexByte(s, '\''); end > 0 {
			return s[1:end]
		}
		return s
	}
	end := strings.LastIndexByte(s, '"')
	if end <= 0 {
		return strings.Trim(s, `"`)
	}
	body := s[1:end]
	var sb strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' || i+1 >= len(body) {
			sb.WriteByte(body[i])
			continue
		}
		i++
		switch body[i] {
		case 'n':
			sb.WriteByte('\n')
		case 't':
			sb.WriteByte('\t')
		case 'r':
			sb.WriteByte('\r')
		case '\\', '"':
			sb.WriteByte(body[i])
		default:
			sb.WriteByte(body[i])
		}
	}
	return sb.String()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// NewUUID returns a random RFC 4122 version 4 UUID. The CLI generates its own
// cascade and trajectory ids locally in this form rather than having the server
// mint them.
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("generate uuid: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	), nil
}

// MustUUID is NewUUID for call sites that have no meaningful recovery.
func MustUUID() string {
	id, err := NewUUID()
	if err != nil {
		// Fall back to a fixed pattern rather than failing a request outright;
		// collision odds are irrelevant because the server does not key on it.
		return "00000000-0000-4000-8000-000000000000"
	}
	return id
}
