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
	httpSrv := &http.Server{
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          newStdLogger(logger),
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	logger.Info("listening", "addr", ln.Addr().String(), "db", cfg.DBPath, "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
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
