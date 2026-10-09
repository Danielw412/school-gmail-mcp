package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type role int

const (
	roleSync   role = iota + 1 // Apps Script: SYNC_SECRET
	roleClient                 // local tools: API_SECRET
)

// Request body limits. Every handler reads through http.MaxBytesReader.
const (
	maxIngestBody        = 10 << 20
	maxActionRequestBody = 4 << 20
	maxListBody          = 1 << 20
	maxSmallBody         = 64 << 10
	maxConcurrent        = 16
)

type Server struct {
	cfg      Config
	store    *Store
	log      *slog.Logger
	now      func() time.Time
	syncHash [32]byte
	apiHash  [32]byte
	sem      chan struct{}

	authLogMu   sync.Mutex
	authLogLast time.Time
	authLogSkip int

	tools     []*mcpTool // MCP tools, built by mcpRoutes
	accountMu sync.Mutex
	account   string // mailbox owner's address, once known

	fetcher   attachmentFetcher // downloads attachments through Apps Script
	relay     *attachmentRelay  // set when fetcher is the poll relay
	conv      *converter
	attachSem chan struct{} // attachment downloads in progress (bounds memory)
}

func newServer(cfg Config, store *Store, logger *slog.Logger, now func() time.Time) *Server {
	s := &Server{
		cfg:       cfg,
		store:     store,
		log:       logger,
		now:       now,
		syncHash:  sha256.Sum256([]byte(cfg.SyncSecret)),
		apiHash:   sha256.Sum256([]byte(cfg.APISecret)),
		sem:       make(chan struct{}, maxConcurrent),
		conv:      &converter{log: logger},
		attachSem: make(chan struct{}, 2),
	}
	if cfg.AttachmentFetchURL != "" {
		s.fetcher = newWebAppFetcher(cfg.AttachmentFetchURL, cfg.AttachmentFetchSecret, cfg.AttachmentFetchTimeout, now)
	} else {
		s.relay = newAttachmentRelay(now)
		s.fetcher = s.relay
	}
	return s
}

func (s *Server) nowMS() int64 { return s.now().UnixMilli() }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// Apps Script (SYNC_SECRET).
	mux.Handle("GET /v1/sync/ping", s.guard(roleSync, 0, s.handlePing))
	mux.Handle("POST /v1/sync/messages", s.guard(roleSync, maxIngestBody, s.handleIngestBatch))
	mux.Handle("POST /v1/sync/message", s.guard(roleSync, maxIngestBody, s.handleIngestOne))
	mux.Handle("POST /v1/sync/known", s.guard(roleSync, maxListBody, s.handleKnown))
	mux.Handle("POST /v1/sync/state", s.guard(roleSync, maxListBody, s.handleState))
	mux.Handle("POST /v1/sync/labels", s.guard(roleSync, maxListBody, s.handleLabels))
	mux.Handle("POST /v1/actions/poll", s.guard(roleSync, maxSmallBody, s.handlePoll))
	mux.Handle("POST /v1/actions/{id}/complete", s.guard(roleSync, maxSmallBody, s.handleComplete))
	mux.Handle("POST /v1/actions/{id}/fail", s.guard(roleSync, maxSmallBody, s.handleFail))
	mux.Handle("POST /v1/sync/attachments", s.guard(roleSync, maxIngestBody, s.handleAttachmentMetadata))
	mux.Handle("POST /v1/attachments/relay/{id}", s.guard(roleSync, relayMaxBody(max(s.cfg.AttachmentMaxBytes, 1<<10)), s.handleRelayUpload))

	// Local clients (API_SECRET; refused through the tunnel unless ALLOW_REMOTE_CLIENT_API).
	mux.Handle("POST /v1/actions", s.guard(roleClient, maxActionRequestBody, s.handleCreateAction))
	mux.Handle("GET /v1/actions", s.guard(roleClient, 0, s.handleListActions))
	mux.Handle("GET /v1/actions/{id}", s.guard(roleClient, 0, s.handleGetAction))
	mux.Handle("POST /v1/actions/{id}/cancel", s.guard(roleClient, maxSmallBody, s.handleCancel))
	mux.Handle("GET /v1/messages", s.guard(roleClient, 0, s.handleSearch))
	mux.Handle("GET /v1/messages/{id}", s.guard(roleClient, 0, s.handleGetMessage))
	mux.Handle("GET /v1/threads/{id}", s.guard(roleClient, 0, s.handleGetThread))
	mux.Handle("GET /v1/stats", s.guard(roleClient, 0, s.handleStats))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return s.middleware(mux)
}

// ---------------------------------------------------------------------------
// Middleware

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
	quiet  bool
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// markQuiet demotes the access-log line for routine traffic (idle polls, health
// checks) to debug so the journal is not filled with one line per minute.
func markQuiet(w http.ResponseWriter) {
	if sw, ok := w.(*statusWriter); ok {
		sw.quiet = true
	}
}

func clientVia(r *http.Request) string {
	if ip := r.Header.Get("Cf-Connecting-Ip"); ip != "" {
		return "cf:" + ip
	}
	return "local"
}

// viaProxy reports whether the request came through Cloudflare. The edge always
// adds these headers and clients cannot remove them; cloudflared passes them on.
func viaProxy(r *http.Request) bool {
	for _, h := range []string{"Cf-Connecting-Ip", "Cf-Ray", "Cf-Warp-Tag-Id", "X-Forwarded-For", "Forwarded"} {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		default:
			w.Header().Set("Retry-After", "5")
			writeError(sw, http.StatusServiceUnavailable, "busy", "too many concurrent requests")
			s.log.Warn("http busy", "method", r.Method, "path", r.URL.Path, "via", clientVia(r))
			return
		}
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.log.Error("panic", "method", r.Method, "path", r.URL.Path, "err", fmt.Sprint(rec))
				if !sw.wrote {
					writeError(sw, http.StatusInternalServerError, "internal", "internal error")
				}
			}
			level := slog.LevelInfo
			if sw.quiet && sw.status < 400 {
				level = slog.LevelDebug
			}
			// Paths only: query strings can contain search terms.
			s.log.Log(r.Context(), level, "http", "method", r.Method, "path", r.URL.Path, "status", sw.status,
				"ms", time.Since(start).Milliseconds(), "in", r.ContentLength, "out", sw.bytes, "via", clientVia(r))
		}()
		next.ServeHTTP(sw, r)
	})
}

func (s *Server) authorized(r *http.Request, want role) bool {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(h[len(prefix):])))
	expected := s.syncHash
	if want == roleClient {
		expected = s.apiHash
	}
	return subtle.ConstantTimeCompare(sum[:], expected[:]) == 1
}

// logAuthFailure logs at most one line per 10 seconds so a scan cannot flood the journal.
func (s *Server) logAuthFailure(r *http.Request) {
	s.authLogMu.Lock()
	defer s.authLogMu.Unlock()
	if time.Since(s.authLogLast) < 10*time.Second {
		s.authLogSkip++
		return
	}
	s.log.Warn("auth rejected", "method", r.Method, "path", r.URL.Path, "via", clientVia(r), "suppressed", s.authLogSkip)
	s.authLogLast, s.authLogSkip = time.Now(), 0
}

func hasBody(r *http.Request) bool {
	return r.ContentLength > 0 || r.ContentLength == -1
}

func (s *Server) guard(want role, maxBody int64, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r, want) {
			s.logAuthFailure(r)
			w.Header().Set("WWW-Authenticate", `Bearer realm="school-email-sync"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		if want == roleClient && !s.cfg.AllowRemoteAPI && viaProxy(r) {
			writeError(w, http.StatusForbidden, "local_only", "this endpoint is only available from the server itself")
			return
		}
		if r.Method == http.MethodPost && hasBody(r) {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
				return
			}
		}
		if maxBody > 0 && r.ContentLength > maxBody {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("request body exceeds %d bytes", maxBody))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, max(maxBody, 1))
		h(w, r)
	})
}

// ---------------------------------------------------------------------------
// Helpers

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg}})
}

func (s *Server) badRequest(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("request body exceeds %d bytes", tooBig.Limit))
		return
	}
	writeError(w, http.StatusBadRequest, "bad_request", err.Error())
}

func (s *Server) storeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, errStaleClaim):
		writeError(w, http.StatusConflict, "stale_claim", err.Error())
	case errors.Is(err, errConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, errBadQuery):
		writeError(w, http.StatusBadRequest, "bad_query", err.Error())
	case errors.Is(err, context.Canceled):
		writeError(w, 499, "canceled", "request canceled")
	default:
		s.log.Error("store error", "path", r.URL.Path, "err", err.Error())
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// decodeJSON decodes exactly one JSON object, rejecting unknown fields and trailing data.
func decodeJSON(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("request body is empty")
		}
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return err
		}
		return errors.New("invalid JSON: unexpected data after the object")
	}
	return nil
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf(`invalid JSON: expected {"messages": [ ... ]}`)
	}
	return nil
}

// decodeMessageBatch streams {"messages":[...]} one element at a time, so peak
// memory is the decoded batch rather than raw body plus decoded batch.
func decodeMessageBatch(r io.Reader) ([]MessageIn, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if key, ok := tok.(string); !ok || key != "messages" {
		return nil, errors.New(`invalid JSON: expected {"messages": [ ... ]}`)
	}
	if err := expectDelim(dec, '['); err != nil {
		return nil, err
	}
	var msgs []MessageIn
	for dec.More() {
		if len(msgs) == maxBatchMessages {
			return nil, fmt.Errorf("batch has more than %d messages", maxBatchMessages)
		}
		var m MessageIn
		if err := dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", len(msgs), err)
		}
		msgs = append(msgs, m)
	}
	if err := expectDelim(dec, ']'); err != nil {
		return nil, err
	}
	if err := expectDelim(dec, '}'); err != nil {
		return nil, errors.New(`invalid JSON: only the "messages" field is allowed`)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid JSON: unexpected data after the object")
	}
	if len(msgs) == 0 {
		return nil, errors.New("messages must not be empty")
	}
	return msgs, nil
}

// ---------------------------------------------------------------------------
// Handlers: health and Apps Script

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	markQuiet(w)
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		s.log.Error("health check failed", "err", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "role": "sync", "version": version, "serverTime": s.now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request, msgs []MessageIn) {
	sum, err := s.store.IngestMessages(r.Context(), msgs, s.nowMS())
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	s.log.Info("ingest", "received", sum.Received, "inserted", sum.Inserted, "updated", sum.Updated,
		"unchanged", sum.Unchanged, "rejected", sum.Rejected)
	for _, res := range sum.Results {
		if res.Status == "rejected" {
			s.log.Warn("ingest rejected item", "id", res.ID, "reason", res.Error)
		}
	}
	writeJSON(w, http.StatusOK, sum)
}

func (s *Server) handleIngestBatch(w http.ResponseWriter, r *http.Request) {
	msgs, err := decodeMessageBatch(r.Body)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	s.ingest(w, r, msgs)
}

func (s *Server) handleIngestOne(w http.ResponseWriter, r *http.Request) {
	var m MessageIn
	if err := decodeJSON(r.Body, &m); err != nil {
		s.badRequest(w, err)
		return
	}
	s.ingest(w, r, []MessageIn{m})
}

func validateIDList(ids []string) error {
	if len(ids) > maxIDsPerRequest {
		return fmt.Errorf("at most %d ids per request", maxIDsPerRequest)
	}
	for i, id := range ids {
		if !gmailIDPattern.MatchString(id) {
			return fmt.Errorf("ids[%d]: invalid Gmail message id", i)
		}
	}
	return nil
}

func (s *Server) handleKnown(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		s.badRequest(w, err)
		return
	}
	if err := validateIDList(req.IDs); err != nil {
		s.badRequest(w, err)
		return
	}
	unknown, err := s.store.UnknownIDs(r.Context(), req.IDs)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	markQuiet(w)
	writeJSON(w, http.StatusOK, map[string]any{"received": len(req.IDs), "unknown": unknown})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Updates []StateUpdate `json:"updates"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		s.badRequest(w, err)
		return
	}
	if len(req.Updates) == 0 || len(req.Updates) > maxIDsPerRequest {
		s.badRequest(w, fmt.Errorf("updates must contain 1 to %d items", maxIDsPerRequest))
		return
	}
	sum, err := s.store.ApplyStateUpdates(r.Context(), req.Updates, s.nowMS())
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if sum.Updated == 0 && sum.Rejected == 0 {
		markQuiet(w)
	} else {
		s.log.Info("state", "received", sum.Received, "updated", sum.Updated, "unknown", len(sum.Unknown), "rejected", sum.Rejected)
	}
	writeJSON(w, http.StatusOK, sum)
}

func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Labels []LabelIn `json:"labels"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		s.badRequest(w, err)
		return
	}
	if len(req.Labels) > maxLabelsTotal {
		s.badRequest(w, fmt.Errorf("at most %d labels", maxLabelsTotal))
		return
	}
	for i, l := range req.Labels {
		if !labelIDPattern.MatchString(l.ID) || l.Name == "" || len(l.Name) > 512 || len(l.Type) > 16 {
			s.badRequest(w, fmt.Errorf("labels[%d]: invalid id, name or type", i))
			return
		}
	}
	n, err := s.store.ReplaceLabels(r.Context(), req.Labels, s.nowMS())
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stored": n})
}

func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	req := struct {
		Limit int `json:"limit"`
	}{Limit: 5}
	if hasBody(r) {
		if err := decodeJSON(r.Body, &req); err != nil {
			s.badRequest(w, err)
			return
		}
	}
	if req.Limit < 1 || req.Limit > 20 {
		s.badRequest(w, errors.New("limit must be between 1 and 20"))
		return
	}
	actions, err := s.store.ClaimActions(r.Context(), req.Limit, s.nowMS(), s.cfg.ActionLease)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	for _, a := range actions {
		s.log.Info("action claimed", "id", a.ID, "type", a.Type, "attempt", a.Attempt)
	}
	out := map[string]any{"actions": actions, "serverTime": s.now().UTC().Format(time.RFC3339)}
	// Attachment downloads waiting for Apps Script ride along (relay mode only).
	if s.relay != nil {
		if reqs := s.relay.Claim(3); len(reqs) > 0 {
			out["attachmentRequests"] = reqs
			s.log.Info("attachment requests claimed", "count", len(reqs))
		}
	}
	if out["attachmentRequests"] == nil && len(actions) == 0 {
		markQuiet(w)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClaimID string       `json:"claimId"`
		Result  ActionResult `json:"result"`
	}
	if hasBody(r) {
		if err := decodeJSON(r.Body, &req); err != nil {
			s.badRequest(w, err)
			return
		}
	}
	for _, id := range []string{req.Result.MessageID, req.Result.ThreadID} {
		if id != "" && !gmailIDPattern.MatchString(id) {
			s.badRequest(w, errors.New("result: invalid Gmail id"))
			return
		}
	}
	a, already, err := s.store.CompleteAction(r.Context(), r.PathValue("id"), req.Result, s.nowMS())
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if !already {
		s.log.Info("action done", "id", a.ID, "type", a.Type, "attempts", a.Attempts)
	}
	writeJSON(w, http.StatusOK, map[string]any{"action": a, "alreadyDone": already})
}

func (s *Server) handleFail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClaimID   string `json:"claimId"`
		Error     string `json:"error"`
		Retryable bool   `json:"retryable"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		s.badRequest(w, err)
		return
	}
	if req.ClaimID == "" || strings.TrimSpace(req.Error) == "" {
		s.badRequest(w, errors.New("claimId and error are required"))
		return
	}
	a, err := s.store.FailAction(r.Context(), r.PathValue("id"), req.ClaimID, req.Error, req.Retryable, s.nowMS())
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	s.log.Warn("action attempt failed", "id", a.ID, "type", a.Type, "attempt", a.Attempts,
		"retryable", req.Retryable, "status", a.Status)
	writeJSON(w, http.StatusOK, map[string]any{"action": a})
}

// ---------------------------------------------------------------------------
// Handlers: local client API

func (s *Server) handleCreateAction(w http.ResponseWriter, r *http.Request) {
	var req ActionRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		s.badRequest(w, err)
		return
	}
	payload, err := req.toPayload()
	if err != nil {
		s.badRequest(w, err)
		return
	}
	a, created, err := s.store.CreateAction(r.Context(), req.Type, payload, req.IdempotencyKey, s.cfg.ActionMaxAttempts, s.nowMS(), 0)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		s.log.Info("action queued", "id", a.ID, "type", a.Type)
	}
	writeJSON(w, status, map[string]any{"action": a, "created": created})
}

var actionStatuses = map[string]bool{"pending": true, "claimed": true, "done": true, "failed": true, "cancelled": true}

func (s *Server) handleListActions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	if status != "" && !actionStatuses[status] {
		s.badRequest(w, errors.New("status must be one of pending, claimed, done, failed, cancelled"))
		return
	}
	limit, err := intParam(q, "limit", 50, 1, 200)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	list, err := s.store.ListActions(r.Context(), status, limit)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"actions": list})
}

func (s *Server) handleGetAction(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.GetAction(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"action": a})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.CancelAction(r.Context(), r.PathValue("id"), s.nowMS())
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	s.log.Info("action cancelled", "id", a.ID)
	writeJSON(w, http.StatusOK, map[string]any{"action": a})
}

func intParam(q url.Values, name string, def, lo, hi int) (int, error) {
	v := q.Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, lo, hi)
	}
	return n, nil
}

func boolParam(q url.Values, name string) (*bool, error) {
	v := q.Get(name)
	if v == "" {
		return nil, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return nil, fmt.Errorf("%s must be true or false", name)
	}
	return &b, nil
}

// parseTimeParam accepts Unix seconds or milliseconds, RFC 3339, or YYYY-MM-DD (UTC).
func parseTimeParam(v string) (int64, error) {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 100_000_000_000 {
			n *= 1000
		}
		return n, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UnixMilli(), nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.UnixMilli(), nil
	}
	return 0, fmt.Errorf("invalid time %q (use RFC 3339, YYYY-MM-DD or Unix time)", v)
}

func parseSearch(q url.Values) (SearchParams, error) {
	p := SearchParams{
		Text: q.Get("q"), FTS: q.Get("fts"), From: q.Get("from"), To: q.Get("to"),
		Subject: q.Get("subject"), Label: q.Get("label"), ThreadID: q.Get("threadId"),
	}
	for _, v := range []string{p.Text, p.FTS, p.From, p.To, p.Subject, p.Label, p.ThreadID} {
		if len(v) > 500 {
			return p, errors.New("query parameters must be at most 500 bytes")
		}
	}
	var err error
	if p.Unread, err = boolParam(q, "unread"); err != nil {
		return p, err
	}
	if p.Starred, err = boolParam(q, "starred"); err != nil {
		return p, err
	}
	if p.InInbox, err = boolParam(q, "inbox"); err != nil {
		return p, err
	}
	if b, err := boolParam(q, "includeTrash"); err != nil {
		return p, err
	} else if b != nil {
		p.WithTrash = *b
	}
	for name, dst := range map[string]*int64{"after": &p.After, "before": &p.Before} {
		if v := q.Get(name); v != "" {
			if *dst, err = parseTimeParam(v); err != nil {
				return p, fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	if p.Limit, err = intParam(q, "limit", 20, 1, 100); err != nil {
		return p, err
	}
	if c := q.Get("cursor"); c != "" {
		ts, id, ok := strings.Cut(c, ":")
		n, err := strconv.ParseInt(ts, 10, 64)
		if !ok || err != nil || !gmailIDPattern.MatchString(id) {
			return p, errors.New("cursor: invalid (use nextCursor from a previous response)")
		}
		p.CursorTS, p.CursorID = n, id
	}
	return p, nil
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	p, err := parseSearch(r.URL.Query())
	if err != nil {
		s.badRequest(w, err)
		return
	}
	msgs, next, err := s.store.Search(r.Context(), p)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	resp := map[string]any{"messages": msgs}
	if next != "" {
		resp["nextCursor"] = next
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	withHTML, err := boolParam(r.URL.Query(), "html")
	if err != nil {
		s.badRequest(w, err)
		return
	}
	m, err := s.store.GetMessage(r.Context(), r.PathValue("id"), withHTML != nil && *withHTML)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": m})
}

func (s *Server) handleGetThread(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := intParam(q, "limit", 50, 1, 100)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	bodyChars, err := intParam(q, "bodyChars", 20000, 0, 200000)
	if err != nil {
		s.badRequest(w, err)
		return
	}
	msgs, err := s.store.GetThread(r.Context(), r.PathValue("id"), limit, bodyChars)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"threadId": r.PathValue("id"), "messages": msgs})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.Stats(r.Context())
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
