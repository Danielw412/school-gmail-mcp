// Command school-email-sync is the Linux side of the school Gmail sync system.
//
// It stores Gmail messages pushed by the Google Apps Script half, queues outbound
// actions (send/reply) for Apps Script to perform, and answers small authenticated
// queries. It listens on loopback only; Cloudflare Tunnel provides public TLS.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

const usageText = `usage: school-email-sync <command>

commands:
  serve              run the HTTP backend (configuration comes from the environment)
  backup [-db PATH] DEST
                     write a consistent snapshot of the database to DEST (safe while serving)
  gen-secret         print a new random 256-bit secret (hex)
  version            print the version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe()
	case "backup":
		err = cmdBackup(os.Args[2:])
	case "gen-secret":
		err = cmdGenSecret()
	case "version", "-v", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usageText)
	default:
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func cmdServe() error {
	// The database, its WAL and any temp files must not be readable by other users.
	syscall.Umask(0o077)

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)

	store, err := openStore(cfg.DBPath)
	if err != nil {
		return err
	}
	defer store.Close()

	srv := newServer(cfg, store, logger, time.Now)
	httpSrv := newHTTPServer(srv.routes(), logger, 60*time.Second)

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	logger.Info("listening", "addr", ln.Addr().String(), "db", cfg.DBPath, "version", version)
	servers := []*http.Server{httpSrv}
	listeners := []net.Listener{ln}

	// The MCP endpoint gets its own loopback port: only LISTEN_ADDR is routed by
	// Cloudflare, so the mailbox tools are reachable solely via the OpenAI tunnel.
	if cfg.MCPListenAddr != "" {
		mcpLn, err := net.Listen("tcp", cfg.MCPListenAddr)
		if err != nil {
			ln.Close()
			return fmt.Errorf("MCP_LISTEN_ADDR: %w", err)
		}
		// get_attachment may wait for Gmail and then convert the file.
		mcpWrite := max(60*time.Second, cfg.AttachmentFetchTimeout+cfg.AttachmentConvertTimeout+15*time.Second)
		servers = append(servers, newHTTPServer(srv.mcpRoutes(), logger, mcpWrite))
		listeners = append(listeners, mcpLn)
		logger.Info("mcp listening", "addr", mcpLn.Addr().String(), "send_enabled", cfg.MCPAllowSend,
			"send_delay_s", int(cfg.MCPSendDelay.Seconds()), "attachments", srv.fetcher.Mode(),
			"attachment_max_bytes", cfg.AttachmentMaxBytes)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, len(servers))
	for i := range servers {
		go func(s *http.Server, l net.Listener) { errCh <- s.Serve(l) }(servers[i], listeners[i])
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil && serveErr == nil {
			serveErr = err
		}
	}
	return serveErr
}

func newHTTPServer(h http.Handler, logger *slog.Logger, writeTimeout time.Duration) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          newStdLogger(logger),
	}
}

func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	dbPath := fs.String("db", "", "database path (default: DB_PATH, then $STATE_DIRECTORY/mail.db, then ~/.local/state/school-email-sync/mail.db)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: school-email-sync backup [-db PATH] DEST")
	}
	path := *dbPath
	if path == "" {
		path = defaultDBPath(os.Getenv, true)
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("database not found: %w", err)
	}
	syscall.Umask(0o077)
	store, err := openStore(path)
	if err != nil {
		return err
	}
	defer store.Close()
	dest := fs.Arg(0)
	if err := store.Backup(context.Background(), dest); err != nil {
		return err
	}
	fmt.Println("backup written to", dest)
	return nil
}

func cmdGenSecret() error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	fmt.Println(hex.EncodeToString(b))
	return nil
}
