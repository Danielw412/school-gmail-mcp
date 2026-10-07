package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
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
}

const minSecretLen = 32

func loadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		ListenAddr:        "127.0.0.1:8766",
		ActionLease:       10 * time.Minute,
		ActionMaxAttempts: 5,
		LogLevel:          slog.LevelInfo,
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
	return c, nil
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
