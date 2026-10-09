package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is read once from the environment at startup (systemd EnvironmentFile).
type Config struct {
	ListenAddr string
	DBPath     string

	// SyncSecret authenticates the Apps Script side (ingest, action polling, reporting).
	SyncSecret string
	// APISecret authenticates local clients (search, read, creating outbound actions).
	APISecret string
	// AllowRemoteAPI lets the client API be used through the Cloudflare tunnel.
	// Off by default: sending mail should only be requestable from this machine.
	AllowRemoteAPI bool

	ActionLease       time.Duration
	ActionMaxAttempts int
	LogLevel          slog.Level

	// MCPListenAddr is the loopback address of the MCP endpoint for ChatGPT, reached
	// only through the OpenAI Secure MCP Tunnel (never routed by Cloudflare). Empty
	// disables it.
	MCPListenAddr string
	// MCPSendDelay holds email queued through MCP before Apps Script may send it,
	// so a request can still be cancelled ("undo send").
	MCPSendDelay time.Duration
	// MCPAllowSend exposes the send/reply tools. When false, MCP is read-only.
	MCPAllowSend bool

	// AttachmentFetchURL is the Apps Script web app (/exec URL) that downloads one
	// attachment from Gmail when an agent asks for it. Requests are signed with
	// AttachmentFetchSecret. Empty: requests ride on the Apps Script action poll
	// instead (no web app needed, but up to about a minute slower).
	AttachmentFetchURL    string
	AttachmentFetchSecret string
	// AttachmentMaxBytes is the largest attachment that is downloaded and converted.
	AttachmentMaxBytes int64
	// AttachmentFetchTimeout bounds the download from Gmail (or the wait for the
	// poll); AttachmentConvertTimeout bounds text extraction and image conversion.
	AttachmentFetchTimeout   time.Duration
	AttachmentConvertTimeout time.Duration
}

const minSecretLen = 32

func loadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		ListenAddr:        "127.0.0.1:8766",
		ActionLease:       10 * time.Minute,
		ActionMaxAttempts: 5,
		LogLevel:          slog.LevelInfo,
		MCPListenAddr:     "127.0.0.1:8767",
		MCPSendDelay:      60 * time.Second,
		MCPAllowSend:      true,

		AttachmentMaxBytes:       10 << 20,
		AttachmentFetchTimeout:   40 * time.Second,
		AttachmentConvertTimeout: 15 * time.Second,
	}
	if v := strings.TrimSpace(getenv("LISTEN_ADDR")); v != "" {
		c.ListenAddr = v
	}
	if err := requireLoopback(c.ListenAddr); err != nil {
		return c, err
	}

	c.DBPath = defaultDBPath(getenv, false)
	if strings.ContainsAny(c.DBPath, "?#") {
		return c, errors.New("DB_PATH must not contain '?' or '#'")
	}

	c.SyncSecret = strings.TrimSpace(getenv("SYNC_SECRET"))
	c.APISecret = strings.TrimSpace(getenv("API_SECRET"))
	if len(c.SyncSecret) < minSecretLen {
		return c, fmt.Errorf("SYNC_SECRET must be set and at least %d characters (generate one with: school-email-sync gen-secret)", minSecretLen)
	}
	if len(c.APISecret) < minSecretLen {
		return c, fmt.Errorf("API_SECRET must be set and at least %d characters (generate one with: school-email-sync gen-secret)", minSecretLen)
	}
	if c.SyncSecret == c.APISecret {
		return c, errors.New("SYNC_SECRET and API_SECRET must be different")
	}

	if v := strings.TrimSpace(getenv("ALLOW_REMOTE_CLIENT_API")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("ALLOW_REMOTE_CLIENT_API: %w", err)
		}
		c.AllowRemoteAPI = b
	}
	if v := strings.TrimSpace(getenv("ACTION_LEASE_SECONDS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 60 || n > 3600 {
			return c, errors.New("ACTION_LEASE_SECONDS must be an integer between 60 and 3600")
		}
		c.ActionLease = time.Duration(n) * time.Second
	}
	if v := strings.TrimSpace(getenv("ACTION_MAX_ATTEMPTS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 20 {
			return c, errors.New("ACTION_MAX_ATTEMPTS must be an integer between 1 and 20")
		}
		c.ActionMaxAttempts = n
	}
	if v := strings.TrimSpace(getenv("LOG_LEVEL")); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return c, fmt.Errorf("LOG_LEVEL: %w", err)
		}
	}

	if v := strings.TrimSpace(getenv("MCP_LISTEN_ADDR")); v != "" {
		c.MCPListenAddr = v
		if strings.EqualFold(v, "off") {
			c.MCPListenAddr = ""
		}
	}
	if c.MCPListenAddr != "" {
		if err := requireLoopback(c.MCPListenAddr); err != nil {
			return c, fmt.Errorf("MCP_LISTEN_ADDR: %w", err)
		}
		if c.MCPListenAddr == c.ListenAddr {
			return c, errors.New("MCP_LISTEN_ADDR must differ from LISTEN_ADDR (only LISTEN_ADDR is published through Cloudflare)")
		}
	}
	if v := strings.TrimSpace(getenv("MCP_SEND_DELAY_SECONDS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 900 {
			return c, errors.New("MCP_SEND_DELAY_SECONDS must be an integer between 0 and 900")
		}
		c.MCPSendDelay = time.Duration(n) * time.Second
	}
	if v := strings.TrimSpace(getenv("MCP_ALLOW_SEND")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("MCP_ALLOW_SEND: %w", err)
		}
		c.MCPAllowSend = b
	}

	if v := strings.TrimSpace(getenv("ATTACHMENT_FETCH_URL")); v != "" {
		if err := checkFetchURL(v); err != nil {
			return c, err
		}
		c.AttachmentFetchURL = v
		c.AttachmentFetchSecret = strings.TrimSpace(getenv("ATTACHMENT_FETCH_SECRET"))
		if len(c.AttachmentFetchSecret) < minSecretLen {
			return c, fmt.Errorf("ATTACHMENT_FETCH_SECRET must be set and at least %d characters when ATTACHMENT_FETCH_URL is set", minSecretLen)
		}
		if c.AttachmentFetchSecret == c.SyncSecret || c.AttachmentFetchSecret == c.APISecret {
			return c, errors.New("ATTACHMENT_FETCH_SECRET must differ from SYNC_SECRET and API_SECRET")
		}
	}
	if v := strings.TrimSpace(getenv("ATTACHMENT_MAX_BYTES")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1<<10 || n > 25<<20 {
			return c, errors.New("ATTACHMENT_MAX_BYTES must be an integer between 1024 and 26214400 (25 MiB, Gmail's limit)")
		}
		c.AttachmentMaxBytes = n
	}
	for name, dst := range map[string]*time.Duration{
		"ATTACHMENT_FETCH_TIMEOUT_SECONDS": &c.AttachmentFetchTimeout, "ATTACHMENT_CONVERT_TIMEOUT_SECONDS": &c.AttachmentConvertTimeout,
	} {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 5 || n > 300 {
				return c, fmt.Errorf("%s must be an integer between 5 and 300", name)
			}
			*dst = time.Duration(n) * time.Second
		}
	}
	return c, nil
}

// checkFetchURL accepts an Apps Script web app URL (https://script.google.com/.../exec)
// or, for tests, plain HTTP on loopback.
func checkFetchURL(v string) error {
	u, err := url.Parse(v)
	switch {
	case err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "":
	case u.Scheme == "https" && u.Host == "script.google.com" && strings.HasSuffix(u.Path, "/exec"):
		return nil
	case u.Scheme == "http" && isLoopbackHost(u.Host):
		return nil
	}
	return errors.New("ATTACHMENT_FETCH_URL must be the web app URL https://script.google.com/macros/s/<id>/exec (Deploy -> Manage deployments in Apps Script)")
}

// defaultDBPath resolves DB_PATH, then systemd's $STATE_DIRECTORY, then (for the
// backup command run from a shell) the service's state directory, then ./data.
func defaultDBPath(getenv func(string) string, preferServiceDir bool) string {
	if v := strings.TrimSpace(getenv("DB_PATH")); v != "" {
		return v
	}
	if sd := getenv("STATE_DIRECTORY"); sd != "" {
		return filepath.Join(strings.Split(sd, ":")[0], "mail.db")
	}
	if preferServiceDir {
		if home, err := os.UserHomeDir(); err == nil {
			p := filepath.Join(home, ".local", "state", "school-email-sync", "mail.db")
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return filepath.Join("data", "mail.db")
}

// requireLoopback refuses to listen anywhere but a loopback IP. Public access goes
// through cloudflared, which connects to the backend over loopback.
func requireLoopback(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("LISTEN_ADDR %q: %w", addr, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("LISTEN_ADDR %q: invalid port", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("LISTEN_ADDR %q: must be a loopback IP such as 127.0.0.1 (the backend is never exposed directly)", addr)
	}
	return nil
}

// newLogger writes logfmt lines to stderr without timestamps; journald adds those.
func newLogger(level slog.Level) *slog.Logger {
	return newLoggerTo(os.Stderr, level)
}

func newLoggerTo(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

// newStdLogger routes net/http's internal error log (TLS noise, bad requests) to slog.
func newStdLogger(l *slog.Logger) *log.Logger {
	return slog.NewLogLogger(l.Handler(), slog.LevelWarn)
}
