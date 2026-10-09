package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Attachment content is never stored. When an agent asks for an attachment, the
// backend has Apps Script (which holds the only Gmail credentials) download that
// one file, converts it in memory for the answer and drops it. Two ways to reach
// Apps Script:
//
//   - webAppFetcher: a signed POST to the script's web app (doPost in Code.gs).
//     Answers in seconds. Needs a web app deployment that anyone can call, which
//     some Google Workspace domains do not allow.
//   - attachmentRelay: the request is handed to Apps Script with the next action
//     poll (once a minute) and Apps Script uploads the file over the existing
//     authenticated sync channel. Works under any Workspace policy, but slower.

type attachmentFetcher interface {
	Fetch(ctx context.Context, messageID, partID string, maxBytes int64) ([]byte, error)
	Mode() string
}

// fetchError is a retrieval problem the agent should see as is.
func fetchError(format string, a ...any) error { return toolError{fmt.Sprintf(format, a...)} }

// gasError turns an error code reported by Code.gs into a message for the agent.
func gasError(code, msg string) error {
	switch code {
	case "not_found":
		return fetchError("Gmail no longer has this attachment (the email may have been deleted)")
	case "too_large":
		return fetchError("the attachment is larger than the limit (ATTACHMENT_MAX_BYTES)")
	case "unauthorized":
		return fetchError("Apps Script rejected the request signature: ATTACHMENT_FETCH_SECRET in backend.env must equal the Script Property of the same name")
	case "expired":
		return fetchError("Apps Script rejected the request as expired: this server's clock is off (check timedatectl)")
	case "replayed":
		return fetchError("Apps Script rejected a repeated request; try again")
	case "not_configured":
		return fetchError("Apps Script is not set up for attachments: %s", msg)
	case "unavailable":
		return fetchError("Gmail is temporarily unavailable (%s); try again in a minute", msg)
	}
	return fetchError("Apps Script could not download the attachment: %s", msg)
}

// ---------------------------------------------------------------------------
// Web app

const (
	fetchRequestLabel  = "school-email-sync attachment request v1"
	fetchResponseLabel = "school-email-sync attachment response v1"
)

type webAppFetcher struct {
	url    string
	secret []byte
	client *http.Client
	now    func() time.Time
}

var errWebAppSignIn = errors.New("web app redirected to a Google sign-in page")

func newWebAppFetcher(rawURL, secret string, timeout time.Duration, now func() time.Time) *webAppFetcher {
	home, _ := url.Parse(rawURL)
	client := &http.Client{
		Timeout: timeout,
		// Apps Script answers a POST with a redirect to script.googleusercontent.com,
		// where the result is fetched with GET. A redirect anywhere else means the
		// deployment wants a signed-in Google user.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			h := req.URL.Hostname()
			if (req.URL.Scheme == "https" && (h == "script.google.com" || h == "script.googleusercontent.com")) ||
				(home != nil && req.URL.Host == home.Host) {
				return nil
			}
			return errWebAppSignIn
		},
	}
	return &webAppFetcher{url: rawURL, secret: []byte(secret), client: client, now: now}
}

func (f *webAppFetcher) Mode() string { return "Apps Script web app" }

func (f *webAppFetcher) sign(parts ...string) string {
	m := hmac.New(sha256.New, f.secret)
	m.Write([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(m.Sum(nil))
}

type webAppResponse struct {
	OK       bool   `json:"ok"`
	Nonce    string `json:"nonce"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Sig      string `json:"sig"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
	Filename string `json:"filename"`
	Error    struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (f *webAppFetcher) Fetch(ctx context.Context, messageID, partID string, maxBytes int64) ([]byte, error) {
	ts := strconv.FormatInt(f.now().UnixMilli(), 10)
	nonce := randomHex(16)
	limit := strconv.FormatInt(maxBytes, 10)
	body, _ := json.Marshal(map[string]any{
		"v": 1, "ts": json.Number(ts), "nonce": nonce, "messageId": messageID, "attachmentId": partID,
		"maxBytes": maxBytes, "sig": f.sign(fetchRequestLabel, ts, nonce, messageID, partID, limit),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, errWebAppSignIn):
			return nil, fetchError("the Apps Script web app asks for a Google sign-in, so this server cannot call it. Deploy it with \"Who has access: Anyone\"; " +
				"if your Workspace admin does not allow that, remove ATTACHMENT_FETCH_URL from backend.env to fetch through the sync trigger instead (see README)")
		case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Client.Timeout"):
			return nil, fetchError("Gmail did not deliver the attachment within %s; try again", f.client.Timeout)
		}
		return nil, fmt.Errorf("attachment web app: %w", err)
	}
	defer resp.Body.Close()
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK || mt != "application/json" {
		// Apps Script shows an HTML page for a missing deployment or a script error.
		return nil, fetchError("the Apps Script web app answered HTTP %d (%s) instead of JSON. Check that ATTACHMENT_FETCH_URL is the current "+
			"deployment's /exec URL and that the deployed version includes doPost (see apps-script/README.md)", resp.StatusCode, mt)
	}
	// base64 grows the file by a third; allow some room for the JSON around it.
	var r webAppResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBytes/3*4+(1<<20))).Decode(&r); err != nil {
		return nil, fmt.Errorf("attachment web app: invalid response: %w", err)
	}
	if !r.OK {
		return nil, gasError(r.Error.Code, r.Error.Message)
	}
	sum := sha256.Sum256([]byte(r.Data))
	digest := hex.EncodeToString(sum[:])
	want := f.sign(fetchResponseLabel, nonce, strconv.FormatInt(r.Size, 10), digest)
	if r.Nonce != nonce || r.SHA256 != digest || !hmac.Equal([]byte(want), []byte(r.Sig)) {
		return nil, errors.New("attachment web app: response signature does not verify (wrong ATTACHMENT_FETCH_URL, or the file was altered)")
	}
	data, err := base64.StdEncoding.DecodeString(r.Data)
	r.Data = ""
	if err != nil || int64(len(data)) != r.Size {
		return nil, errors.New("attachment web app: response data is corrupt")
	}
	if int64(len(data)) > maxBytes {
		return nil, fetchError("the attachment is larger than the limit (ATTACHMENT_MAX_BYTES)")
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// Relay through the action poll

const (
	relayMaxPending = 8
	relayIdleTTL    = 90 * time.Second // keep an abandoned request for a retry
	relayReoffer    = 3 * time.Minute  // offer a claimed request again if nothing came back
)

// RelayRequest is what Apps Script receives with the action poll.
type RelayRequest struct {
	ID           string `json:"id"`
	MessageID    string `json:"messageId"`
	AttachmentID string `json:"attachmentId"`
	MaxBytes     int64  `json:"maxBytes"`
}

type relayRequest struct {
	RelayRequest
	claimed, idleSince time.Time
	waiters            int
	done               chan struct{} // closed once data or err is set
	data               []byte
	err                error
}

func (q *relayRequest) finished() bool {
	select {
	case <-q.done:
		return true
	default:
		return false
	}
}

// attachmentRelay hands attachment requests to Apps Script and the uploaded file
// to whoever is waiting for it. A file that arrives when nobody waits any more is
// dropped at once, never kept.
type attachmentRelay struct {
	mu   sync.Mutex
	byID map[string]*relayRequest
	now  func() time.Time
}

func newAttachmentRelay(now func() time.Time) *attachmentRelay {
	return &attachmentRelay{byID: map[string]*relayRequest{}, now: now}
}

func (r *attachmentRelay) Mode() string {
	return "relay through the Apps Script sync (about once a minute)"
}

func (r *attachmentRelay) expireLocked(now time.Time) {
	for id, q := range r.byID {
		if q.waiters == 0 && (q.finished() || now.Sub(q.idleSince) > relayIdleTTL) {
			delete(r.byID, id)
		}
	}
}

func (r *attachmentRelay) Fetch(ctx context.Context, messageID, partID string, maxBytes int64) ([]byte, error) {
	r.mu.Lock()
	now := r.now()
	r.expireLocked(now)
	var q *relayRequest
	for _, p := range r.byID {
		if p.MessageID == messageID && p.AttachmentID == partID && p.MaxBytes == maxBytes && !p.finished() {
			q = p // a retry joins the request that is already queued
			break
		}
	}
	if q == nil {
		if len(r.byID) >= relayMaxPending {
			r.mu.Unlock()
			return nil, fetchError("too many attachment requests are waiting for Apps Script; try again in a minute")
		}
		q = &relayRequest{
			RelayRequest: RelayRequest{ID: "att_" + randomHex(8), MessageID: messageID, AttachmentID: partID, MaxBytes: maxBytes},
			done:         make(chan struct{}),
		}
		r.byID[q.ID] = q
	}
	q.waiters++
	r.mu.Unlock()

	select {
	case <-q.done:
		r.mu.Lock()
		defer r.mu.Unlock()
		q.waiters--
		data, err := q.data, q.err
		if q.waiters == 0 {
			q.data = nil
			delete(r.byID, q.ID)
		}
		return data, err
	case <-ctx.Done():
		r.mu.Lock()
		defer r.mu.Unlock()
		q.waiters--
		if q.waiters == 0 {
			q.idleSince = r.now()
		}
		if !q.claimed.IsZero() {
			return nil, fetchError("Apps Script is still downloading the attachment from Gmail. Call get_attachment again with the same arguments in a few seconds")
		}
		return nil, fetchError("the Gmail sync (Apps Script, runs about once a minute) has not picked up the request yet. " +
			"Call get_attachment again with the same arguments in about 30 seconds; if this keeps happening, check mailbox_status " +
			"(the sync must be running and Code.gs up to date)")
	}
}

// Claim returns requests for Apps Script to fetch, at most limit, and marks them claimed.
func (r *attachmentRelay) Claim(limit int) []RelayRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.expireLocked(now)
	out := []RelayRequest{}
	for _, q := range r.byID {
		if len(out) == limit {
			break
		}
		if q.finished() || (!q.claimed.IsZero() && now.Sub(q.claimed) < relayReoffer) {
			continue
		}
		q.claimed = now
		out = append(out, q.RelayRequest)
	}
	return out
}

var errRelayGone = errors.New("nobody is waiting for this attachment any more")

// Deliver passes an uploaded file (or Apps Script's error) to the waiting request.
func (r *attachmentRelay) Deliver(id string, data []byte, err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.byID[id]
	if q == nil || q.finished() {
		return errNotFound
	}
	if q.waiters == 0 {
		delete(r.byID, id) // the agent gave up: drop the file now rather than keep it
		return errRelayGone
	}
	q.data, q.err = data, err
	close(q.done)
	return nil
}

// relayUpload is the body Apps Script posts to /v1/attachments/relay/{id}.
type relayUpload struct {
	OK       bool   `json:"ok"`
	Data     string `json:"data"`
	Size     int64  `json:"size"`
	MimeType string `json:"mimeType"`
	Filename string `json:"filename"`
	Error    struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func relayMaxBody(maxBytes int64) int64 { return maxBytes/3*4 + (1 << 20) }

func (s *Server) handleRelayUpload(w http.ResponseWriter, r *http.Request) {
	if s.relay == nil {
		writeError(w, http.StatusNotFound, "not_found", "attachment relay is off (ATTACHMENT_FETCH_URL is set)")
		return
	}
	var up relayUpload
	if err := decodeJSON(r.Body, &up); err != nil {
		s.badRequest(w, err)
		return
	}
	var data []byte
	var fetchErr error
	if up.OK {
		var err error
		data, err = base64.StdEncoding.DecodeString(up.Data)
		up.Data = ""
		if err != nil || int64(len(data)) != up.Size {
			s.badRequest(w, errors.New("data: invalid base64 or size mismatch"))
			return
		}
	} else {
		fetchErr = gasError(up.Error.Code, truncateUTF8(up.Error.Message, 300))
	}
	switch err := s.relay.Deliver(r.PathValue("id"), data, fetchErr); {
	case errors.Is(err, errRelayGone):
		writeError(w, http.StatusGone, "gone", err.Error())
	case err != nil:
		writeError(w, http.StatusNotFound, "not_found", "no such attachment request")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"delivered": true})
	}
}

// ---------------------------------------------------------------------------
// Backfill of attachment metadata (Apps Script startAttachmentBackfill)

func (s *Server) handleAttachmentMetadata(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []MessageAttachments `json:"messages"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		s.badRequest(w, err)
		return
	}
	if len(req.Messages) == 0 || len(req.Messages) > maxBatchMessages {
		s.badRequest(w, fmt.Errorf("messages must contain 1 to %d items", maxBatchMessages))
		return
	}
	sum, err := s.store.ReplaceAttachments(r.Context(), req.Messages, s.nowMS())
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	s.log.Info("attachment metadata", "received", sum.Received, "updated", sum.Updated, "unknown", len(sum.Unknown), "rejected", sum.Rejected)
	writeJSON(w, http.StatusOK, sum)
}
