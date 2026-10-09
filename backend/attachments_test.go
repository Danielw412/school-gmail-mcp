package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// attEnv serves one Server on both ports: the Apps Script API and MCP.
type attEnv struct {
	api *testEnv
	mcp *mcpEnv
	srv *Server
}

func newAttEnv(t *testing.T, mutate func(*Config)) *attEnv {
	t.Helper()
	store, err := openStore(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ListenAddr: "127.0.0.1:0", SyncSecret: testSyncSecret, APISecret: testAPISecret,
		ActionLease: 10 * time.Minute, ActionMaxAttempts: 3, MCPListenAddr: "127.0.0.1:0", MCPAllowSend: true,
		AttachmentMaxBytes: 10 << 20, AttachmentFetchTimeout: 5 * time.Second, AttachmentConvertTimeout: 15 * time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	clock := &fakeClock{t: time.Date(2026, 10, 6, 16, 0, 0, 0, time.UTC)}
	srv := newServer(cfg, store, newLoggerTo(io.Discard, slog.LevelError), clock.Now)
	apiTS := httptest.NewServer(srv.routes())
	mcpTS := httptest.NewServer(srv.mcpRoutes())
	t.Cleanup(func() {
		apiTS.Close()
		mcpTS.Close()
		store.Close()
	})
	return &attEnv{
		api: &testEnv{t: t, ts: apiTS, store: store, clock: clock},
		mcp: &mcpEnv{t: t, ts: mcpTS, store: store, clock: clock, srv: srv},
		srv: srv,
	}
}

// fakeFetcher stands in for Apps Script: files by "messageID/partID".
type fakeFetcher struct {
	mu    sync.Mutex
	files map[string][]byte
	calls int
}

func (f *fakeFetcher) Mode() string { return "fake" }

func (f *fakeFetcher) Fetch(ctx context.Context, messageID, partID string, maxBytes int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	data, ok := f.files[messageID+"/"+partID]
	if !ok {
		return nil, gasError("not_found", "")
	}
	if int64(len(data)) > maxBytes {
		return nil, gasError("too_large", "")
	}
	return bytes.Clone(data), nil
}

// callResult invokes a tool and returns the raw result (content blocks, isError).
func (e *mcpEnv) callResult(tool string, args any) ([]map[string]any, bool) {
	e.t.Helper()
	out := e.rpc("tools/call", map[string]any{"name": tool, "arguments": args})
	if out["error"] != nil {
		e.t.Fatalf("%s: rpc error %v", tool, out["error"])
	}
	res := obj(out["result"])
	var blocks []map[string]any
	for _, c := range list(res["content"]) {
		blocks = append(blocks, obj(c))
	}
	return blocks, res["isError"] == true
}

// getAttachment calls get_attachment and returns the metadata block, the text
// block (if any) and the image blocks.
func (e *mcpEnv) getAttachment(args map[string]any) (meta map[string]any, text string, images []map[string]any) {
	e.t.Helper()
	blocks, isErr := e.callResult("get_attachment", args)
	if isErr || len(blocks) == 0 || str(blocks[0]["type"]) != "text" {
		e.t.Fatalf("get_attachment %v: %v", args, blocks)
	}
	if err := json.Unmarshal([]byte(str(blocks[0]["text"])), &meta); err != nil {
		e.t.Fatalf("metadata is not JSON: %v", blocks[0])
	}
	for _, b := range blocks[1:] {
		switch str(b["type"]) {
		case "text":
			text = str(b["text"])
		case "image":
			images = append(images, b)
		default:
			e.t.Fatalf("unexpected block %v", b)
		}
	}
	return meta, text, images
}

func (e *mcpEnv) toolError(tool string, args any) string {
	e.t.Helper()
	blocks, isErr := e.callResult(tool, args)
	if !isErr || len(blocks) != 1 {
		e.t.Fatalf("%s %v: expected a tool error, got %v", tool, args, blocks)
	}
	return str(blocks[0]["text"])
}

// ---------------------------------------------------------------------------
// Test files

// makePDF builds a minimal PDF with one page per entry; "" makes a page without text.
func makePDF(pages ...string) []byte {
	type object struct {
		num  int
		body string
	}
	objs := []object{{1, "<< /Type /Catalog /Pages 2 0 R >>"}}
	var kids []string
	n := 3
	var pageObjs []object
	for _, txt := range pages {
		content := ""
		if txt != "" {
			content = fmt.Sprintf("BT /F1 18 Tf 72 720 Td (%s) Tj ET", txt)
		}
		pageObjs = append(pageObjs,
			object{n, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 << /Type /Font /Subtype /Type1 /BaseFont /Helvetica >> >> >> /Contents %d 0 R >>", n+1)},
			object{n + 1, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content)})
		kids = append(kids, fmt.Sprintf("%d 0 R", n))
		n += 2
	}
	objs = append(objs, object{2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages))})
	objs = append(objs, pageObjs...)
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := map[int]int{}
	for _, o := range objs {
		offsets[o.num] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", o.num, o.body)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", n)
	for i := 1; i < n; i++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", n, xref)
	return b.Bytes()
}

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func makeDOCX(t *testing.T) []byte {
	return makeZip(t, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"word/document.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>
<w:p><w:r><w:t>Field trip</w:t></w:r><w:r><w:t xml:space="preserve"> permission form</w:t></w:r></w:p>
<w:p><w:r><w:t>Name:</w:t><w:tab/><w:t>________</w:t><w:br/><w:t>Due Friday &amp; signed</w:t></w:r></w:p>
<w:tbl><w:tr><w:tc><w:p><w:r><w:t>Item</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Cost</w:t></w:r></w:p></w:tc></w:tr>
<w:tr><w:tc><w:p><w:r><w:t>Bus</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>$5</w:t></w:r></w:p></w:tc></w:tr></w:tbl>
</w:body></w:document>`,
	})
}

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), uint8(x + y), 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func needPoppler(t *testing.T) {
	t.Helper()
	for _, p := range []string{"pdftotext", "pdftoppm"} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skip(p + " not installed (poppler-utils)")
		}
	}
}

// ---------------------------------------------------------------------------
// Metadata: classification, listing, search

func attachmentFixture() []MessageIn {
	return []MessageIn{
		sampleMessage("a1", func(m *MessageIn) {
			m.Subject, m.ThreadID = "Worksheets", "thrA"
			m.BodyHTML = ptr(`<p>See attached.</p><img src="cid:logo@school"><img src="CID:Pasted@x">`)
			m.Attachments = []AttachmentIn{
				{PartID: "1", Filename: "worksheet.pdf", MimeType: "application/pdf", Size: 48213},
				{PartID: "2", Filename: "worksheet.pdf", MimeType: "application/pdf", Size: 51000},                 // same name, other file
				{PartID: "3", Filename: "logo.png", MimeType: "image/png", Size: 2048, ContentID: "<logo@school>"}, // cid: in HTML
				{PartID: "4", Filename: "banner.jpg", MimeType: "image/jpeg", Size: 9000, ContentID: "b@x", Inline: true},
				{PartID: "5", Filename: "photo.jpg", MimeType: "image/jpeg", Size: 300000, ContentID: "photo@x"}, // CID not used: real attachment
				{PartID: "6", Filename: "scan.pdf", MimeType: "application/pdf", Size: 7000, ContentID: "s@x", Inline: true},
				{PartID: "7", Filename: "smime.p7s", MimeType: "application/pkcs7-signature", Size: 3000},
				{PartID: "8", Filename: "pasted.png", MimeType: "image/png", Size: 200000, ContentID: "pasted@x"}, // big inline picture
			}
		}),
		sampleMessage("a2", func(m *MessageIn) {
			m.Subject, m.ThreadID, m.InternalDate = "Signature only", "thrA", baseDate+1000
			m.BodyHTML = ptr(`<p>Hi</p><img src="cid:logo2">`)
			m.Attachments = []AttachmentIn{{PartID: "1.2", Filename: "image001.png", MimeType: "image/png", Size: 4000, ContentID: "logo2"}}
		}),
	}
}

func TestAttachmentListingAndClassification(t *testing.T) {
	e := newMCPEnv(t, false)
	if _, err := e.store.IngestMessages(context.Background(), attachmentFixture(), baseDate); err != nil {
		t.Fatal(err)
	}
	out, _, text := e.call("get_email", map[string]any{"message_id": "a1"})
	var ids []string
	for _, a := range list(out["attachments"]) {
		ids = append(ids, str(obj(a)["attachment_id"])+":"+str(obj(a)["filename"]))
	}
	if strings.Join(ids, " ") != "1:worksheet.pdf 2:worksheet.pdf 5:photo.jpg 6:scan.pdf" {
		t.Fatalf("listed attachments: %v\n%s", ids, text)
	}
	inline := list(out["inline_images"])
	if len(inline) != 1 || str(obj(inline[0])["attachment_id"]) != "8" || num(obj(inline[0])["size_bytes"]) != 200000 {
		t.Fatalf("inline_images: %v", inline)
	}

	// Signature images alone do not make an email "have attachments".
	out, _, _ = e.call("search_emails", map[string]any{"query": "has:attachment"})
	if num(out["count"]) != 1 || str(obj(list(out["emails"])[0])["id"]) != "a1" {
		t.Fatalf("has:attachment: %v", out)
	}
	if n := len(list(obj(list(out["emails"])[0])["attachments"])); n != 4 {
		t.Fatalf("search lists %d attachments, want 4", n)
	}
	out, _, _ = e.call("search_emails", map[string]any{"query": "subject:signature"})
	if n := len(list(obj(list(out["emails"])[0])["attachments"])); n != 0 {
		t.Fatalf("signature-only email lists %d attachments", n)
	}
	out, _, _ = e.call("get_thread", map[string]any{"thread_id": "thrA"})
	msgs := list(out["messages"])
	if len(list(obj(msgs[0])["attachments"])) != 4 || len(list(obj(msgs[1])["attachments"])) != 0 ||
		str(obj(list(obj(msgs[0])["attachments"])[1])["mime_type"]) != "application/pdf" {
		t.Fatalf("get_thread attachments: %v", out)
	}
	out, _, _ = e.call("fetch", map[string]any{"id": "a1"})
	if !strings.Contains(str(out["text"]), "worksheet.pdf (attachment_id 2)") || strings.Contains(str(out["text"]), "logo.png") {
		t.Fatalf("fetch attachments line: %s", out["text"])
	}
}

func TestMigrationV2FromV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaV1 + `PRAGMA user_version = 1;
		INSERT INTO messages (gmail_id, thread_id, internal_date, body_html, first_synced_at, last_synced_at, state_changed_at)
		VALUES ('m1', 't1', 1, '<img src="cid:Logo@X">', 1, 1, 1);
		INSERT INTO attachments (gmail_id, part_id, filename, mime_type, size, content_id, is_inline) VALUES
		('m1', '1', 'a.pdf', 'application/pdf', 10, '', 0),
		('m1', '2', 'logo.png', 'image/png', 10, 'logo@x', 0),
		('m1', '3', 'x.pdf', 'application/pdf', 10, 'p@x', 1);`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	s.db.QueryRow("PRAGMA user_version").Scan(&v)
	if v != len(migrations) {
		t.Fatalf("user_version %d", v)
	}
	rows, _ := s.db.Query(`SELECT name FROM pragma_table_info('attachments')`)
	var cols []string
	for rows.Next() {
		var c string
		rows.Scan(&c)
		cols = append(cols, c)
	}
	rows.Close()
	if got := strings.Join(cols, ","); strings.Contains(got, "stored_path") || strings.Contains(got, "fetch_state") || !strings.Contains(got, "hidden") {
		t.Fatalf("columns after migration: %s", got)
	}
	m, err := s.GetMessage(context.Background(), "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	var hidden []string
	for _, a := range m.Attachments {
		if a.Hidden {
			hidden = append(hidden, a.PartID)
		}
	}
	if strings.Join(hidden, ",") != "2" {
		t.Fatalf("hidden after migration: %v", hidden)
	}
}

func TestAttachmentMetadataBackfill(t *testing.T) {
	e := newTestEnv(t)
	m := sampleMessage("b1", func(m *MessageIn) { m.Attachments = nil; m.BodyHTML = ptr(`<img src="cid:sig">`) })
	e.expect(e.do("POST", "/v1/sync/message", testSyncSecret, m), 200)
	atts := []AttachmentIn{
		{PartID: "1", Filename: "notes.docx", MimeType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Size: 1234, AttachmentID: "x1"},
		{PartID: "2", Filename: "sig.png", MimeType: "IMAGE/PNG", Size: 99, ContentID: "<sig>"},
	}
	body := map[string]any{"messages": []any{
		map[string]any{"id": "b1", "attachments": atts},
		map[string]any{"id": "nope", "attachments": []any{}},
		map[string]any{"id": "bad id!", "attachments": []any{}},
	}}
	r := e.expect(e.do("POST", "/v1/sync/attachments", testSyncSecret, body), 200)
	if num(r.body["updated"]) != 1 || num(r.body["rejected"]) != 1 || len(list(r.body["unknown"])) != 1 {
		t.Fatalf("backfill: %s", r.raw)
	}
	got := e.expect(e.do("GET", "/v1/messages/b1", testAPISecret, nil), 200)
	list1 := list(obj(got.body["message"])["attachments"])
	if len(list1) != 2 || obj(list1[1])["hidden"] != true || str(obj(list1[1])["mimeType"]) != "image/png" {
		t.Fatalf("after backfill: %s", got.raw)
	}
	// The same metadata again (Gmail hands out new attachment IDs each read) is a no-op.
	atts[0].AttachmentID = "x2"
	body = map[string]any{"messages": []any{map[string]any{"id": "b1", "attachments": atts}}}
	r = e.expect(e.do("POST", "/v1/sync/attachments", testSyncSecret, body), 200)
	if num(r.body["unchanged"]) != 1 || num(r.body["updated"]) != 0 {
		t.Fatalf("repeat: %s", r.raw)
	}
	e.expect(e.do("POST", "/v1/sync/attachments", testAPISecret, body), 401)
	e.expect(e.do("POST", "/v1/sync/attachments", testSyncSecret, map[string]any{"messages": []any{}}), 400)
}

// ---------------------------------------------------------------------------
// get_attachment

func TestGetAttachmentContent(t *testing.T) {
	e := newMCPEnv(t, false)
	ctx := context.Background()
	big := strings.Repeat("Line of notes. ", 4000) // 60,000 characters
	msg := sampleMessage("g1", func(m *MessageIn) {
		m.Attachments = []AttachmentIn{
			{PartID: "1", Filename: "notes.txt", MimeType: "text/plain", Size: int64(len(big))},
			{PartID: "2", Filename: "form.docx", MimeType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Size: 900},
			{PartID: "3", Filename: "grades.csv", MimeType: "application/octet-stream", Size: 40},
			{PartID: "4", Filename: "chart.png", MimeType: "image/png", Size: 2000},
			{PartID: "5", Filename: "budget.xlsx", MimeType: "application/vnd.ms-excel", Size: 900},
			{PartID: "6", Filename: "huge.zip", MimeType: "application/zip", Size: 11 << 20},
			{PartID: "7", Filename: "chart.png", MimeType: "image/png", Size: 300000}, // same name as 4
			{PartID: "8", Filename: "lecture.mp4", MimeType: "video/mp4", Size: 9 << 20},
		}
	})
	if _, err := e.store.IngestMessages(ctx, []MessageIn{msg}, baseDate); err != nil {
		t.Fatal(err)
	}
	small, large := makePNG(t, 40, 30), makePNG(t, 2400, 1600)
	ff := &fakeFetcher{files: map[string][]byte{
		"g1/1": []byte(big),
		"g1/2": makeDOCX(t),
		"g1/3": {'N', 'a', 'm', 'e', ',', 'N', 'o', 't', 'e', '\n', 'Z', 0xF6, 'e', ',', 'A', '\n'}, // Latin-1
		"g1/4": small,
		"g1/5": makeZip(t, map[string]string{"xl/workbook.xml": "<workbook/>"}),
		"g1/7": large,
	}}
	e.srv.fetcher = ff

	meta, text, imgs := e.getAttachment(map[string]any{"message_id": "g1", "attachment_id": "1"})
	if meta["format"] != "text" || num(meta["text_chars"]) != len(big)-1 || meta["truncated"] != true || // trailing space trimmed
		num(meta["next_offset"]) != 40000 || len([]rune(text)) != 40000 || len(imgs) != 0 ||
		str(meta["resource_uri"]) != "gmail-attachment://g1/1" {
		t.Fatalf("text part 1: %v (%d chars)", meta, len(text))
	}
	meta, text, _ = e.getAttachment(map[string]any{"message_id": "g1", "attachment_id": "1", "offset": 40000})
	if num(meta["offset"]) != 40000 || meta["next_offset"] != nil || len([]rune(text)) != len(big)-1-40000 {
		t.Fatalf("text part 2: %v", meta)
	}

	meta, text, _ = e.getAttachment(map[string]any{"message_id": "g1", "attachment_id": "2"})
	if meta["format"] != "docx" || !strings.Contains(text, "Field trip permission form\nName:\t________\nDue Friday & signed") ||
		!strings.Contains(text, "Item\tCost\nBus\t$5") {
		t.Fatalf("docx: %v %q", meta, text)
	}
	_, text, _ = e.getAttachment(map[string]any{"message_id": "g1", "attachment_id": "3"})
	if text != "Name,Note\nZöe,A" {
		t.Fatalf("latin-1 csv: %q", text)
	}

	meta, text, imgs = e.getAttachment(map[string]any{"message_id": "g1", "attachment_id": "4"})
	if meta["format"] != "image" || text != "" || len(imgs) != 1 || str(imgs[0]["mimeType"]) != "image/png" ||
		str(imgs[0]["data"]) != base64.StdEncoding.EncodeToString(small) {
		t.Fatalf("small image passes through: %v", meta)
	}
	// Same filename, different attachment: the big one is scaled down to a JPEG.
	meta, _, imgs = e.getAttachment(map[string]any{"message_id": "g1", "attachment_id": "7"})
	raw, _ := base64.StdEncoding.DecodeString(str(imgs[0]["data"]))
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || format != "jpeg" || cfg.Width != maxImageSide || cfg.Height != 1045 || str(imgs[0]["mimeType"]) != "image/jpeg" {
		t.Fatalf("large image: %v %v %+v %v", meta, format, cfg, err)
	}

	meta, text, imgs = e.getAttachment(map[string]any{"message_id": "g1", "attachment_id": "5"})
	if meta["format"] != "unsupported" || text != "" || len(imgs) != 0 || !strings.Contains(fmt.Sprint(meta["notes"]), "Excel") {
		t.Fatalf("unsupported: %v", meta)
	}

	calls := ff.calls
	if msg := e.toolError("get_attachment", map[string]any{"message_id": "g1", "attachment_id": "6"}); !strings.Contains(msg, "ATTACHMENT_MAX_BYTES") {
		t.Fatalf("too large: %s", msg)
	}
	meta, _, _ = e.getAttachment(map[string]any{"message_id": "g1", "attachment_id": "8"})
	if meta["format"] != "unsupported" || !strings.Contains(fmt.Sprint(meta["notes"]), "video") {
		t.Fatalf("video: %v", meta)
	}
	if ff.calls != calls {
		t.Fatal("attachments over the limit, and audio/video, must not be downloaded")
	}
	if msg := e.toolError("get_attachment", map[string]any{"message_id": "g1", "attachment_id": "9"}); !strings.Contains(msg, "2 (form.docx)") {
		t.Fatalf("unknown attachment id should list the real ones: %s", msg)
	}
	if msg := e.toolError("get_attachment", map[string]any{"message_id": "zz", "attachment_id": "1"}); !strings.Contains(msg, "search_emails") {
		t.Fatalf("unknown message: %s", msg)
	}
	delete(ff.files, "g1/4")
	if msg := e.toolError("get_attachment", map[string]any{"message_id": "g1", "attachment_id": "4"}); !strings.Contains(msg, "no longer has") {
		t.Fatalf("gone from Gmail: %s", msg)
	}
}

func TestGetAttachmentPDF(t *testing.T) {
	needPoppler(t)
	e := newMCPEnv(t, false)
	msg := sampleMessage("p1", func(m *MessageIn) {
		m.Attachments = []AttachmentIn{
			{PartID: "1", Filename: "syllabus.pdf", MimeType: "application/pdf", Size: 900},
			{PartID: "2", Filename: "scan.pdf", MimeType: "application/pdf", Size: 900},
			{PartID: "3", Filename: "broken.pdf", MimeType: "application/pdf", Size: 900},
		}
	})
	if _, err := e.store.IngestMessages(context.Background(), []MessageIn{msg}, baseDate); err != nil {
		t.Fatal(err)
	}
	e.srv.fetcher = &fakeFetcher{files: map[string][]byte{
		"p1/1": makePDF("Unit 3 reading list", "", "Quiz on Friday"),
		"p1/2": makePDF(""),
		"p1/3": []byte("%PDF-1.4\nthis is not really a pdf"),
	}}

	meta, text, imgs := e.getAttachment(map[string]any{"message_id": "p1", "attachment_id": "1"})
	if meta["format"] != "pdf" || num(meta["pages"]) != 3 || !strings.Contains(text, "--- Page 1 ---\nUnit 3 reading list") ||
		!strings.Contains(text, "--- Page 3 ---\nQuiz on Friday") || len(imgs) != 1 || fmt.Sprint(meta["images"]) != "[page 2]" {
		t.Fatalf("mixed pdf: %v %q %d images", meta, text, len(imgs))
	}
	meta, text, imgs = e.getAttachment(map[string]any{"message_id": "p1", "attachment_id": "2"})
	raw, _ := base64.StdEncoding.DecodeString(str(imgs[0]["data"]))
	if text != "" || len(imgs) != 1 || str(imgs[0]["mimeType"]) != "image/jpeg" || !bytes.HasPrefix(raw, []byte{0xFF, 0xD8}) ||
		!strings.Contains(fmt.Sprint(meta["notes"]), "no text layer") {
		t.Fatalf("scanned pdf: %v", meta)
	}
	meta, _, _ = e.getAttachment(map[string]any{"message_id": "p1", "attachment_id": "3"})
	if meta["format"] != "unsupported" || !strings.Contains(fmt.Sprint(meta["notes"]), "damaged") {
		t.Fatalf("broken pdf: %v", meta)
	}
}

func TestConverterSandboxAndLimits(t *testing.T) {
	needPoppler(t)
	c := &converter{log: newLoggerTo(io.Discard, slog.LevelError)}
	sandboxed := c.sandbox() != ""
	t.Logf("bwrap sandbox in use: %v", sandboxed)
	if sandboxed {
		// Inside the sandbox the converter sees no home directory and no network.
		out, err := c.run(context.Background(), nil, "sh", "-c", "ls /home 2>&1; cat /etc/hostname 2>&1; getent hosts example.com || echo no-network")
		if err != nil || strings.Contains(string(out), "daniel") || !strings.Contains(string(out), "no-network") {
			t.Fatalf("sandbox is leaky: %q %v", out, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := c.run(ctx, nil, "sleep", "5"); err == nil {
		t.Fatal("timeout not enforced")
	}
	if _, err := c.run(context.Background(), nil, "head", "-c", strconv.Itoa(maxConverterOut+10), "/dev/zero"); err == nil {
		t.Fatal("output limit not enforced")
	}
}

func TestSniffAndDecode(t *testing.T) {
	cases := []struct {
		data           []byte
		filename, mime string
		want           string
	}{
		{makePDF("x"), "a.bin", "application/octet-stream", "pdf"},
		{[]byte("hello, world\n"), "notes", "application/octet-stream", "text"},
		{[]byte("a,b\n1,2\n"), "data.csv", "text/csv", "text"},
		{[]byte("{\"a\":1}"), "x.json", "application/json", "text"},
		{[]byte("plain words"), "x.exe", "application/x-msdownload", "other"},
		{[]byte{0, 1, 2, 3, 0, 0}, "x.txt", "text/plain", "other"},
		{append([]byte{0, 0, 0, 24}, []byte("ftypheic\x00\x00\x00\x00")...), "IMG.HEIC", "image/heic", "heic"},
		{makeZip(t, map[string]string{"ppt/presentation.xml": "x"}), "s.pptx", "", "pptx"},
		{[]byte("GIF89a......"), "a.gif", "", "gif"},
	}
	for _, c := range cases {
		if got := sniffKind(c.data, c.filename, c.mime); got != c.want {
			t.Errorf("sniffKind(%s) = %s, want %s", c.filename, got, c.want)
		}
	}
	utf16le := []byte{0xFF, 0xFE, 'H', 0, 'i', 0, 0xAC, 0x20} // "Hi€"
	if got := decodeText(utf16le); got != "Hi€" {
		t.Errorf("utf-16: %q", got)
	}
	if got := decodeText([]byte("\xEF\xBB\xBFa\r\nb")); got != "a\nb" {
		t.Errorf("bom/crlf: %q", got)
	}
	c := &converter{log: newLoggerTo(io.Discard, slog.LevelError), disabled: true}
	out, err := c.convert(context.Background(), []byte("<html><body><h1>Menu</h1><p>Pizza &amp; salad</p></body></html>"), "menu.html", "text/html")
	if err != nil || out.Text != "Menu\nPizza & salad" {
		t.Errorf("html attachment: %q %v", out.Text, err)
	}
	// A zip bomb disguised as .docx: 65 MB of XML that compresses to almost nothing.
	var bomb bytes.Buffer
	zw := zip.NewWriter(&bomb)
	w, _ := zw.Create("word/document.xml")
	chunk := bytes.Repeat([]byte("<w:t>a</w:t>"), 1<<16)
	for n := 0; n <= maxDocxXML; n += len(chunk) {
		w.Write(chunk)
	}
	zw.Close()
	if _, err := docxText(bomb.Bytes()); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("zip bomb (%d bytes compressed): %v", bomb.Len(), err)
	}
}

// ---------------------------------------------------------------------------
// Resources

func TestAttachmentResources(t *testing.T) {
	e := newMCPEnv(t, false)
	msg := sampleMessage("r1", func(m *MessageIn) {
		m.Attachments = []AttachmentIn{
			{PartID: "", Filename: "whole.pdf", MimeType: "application/pdf", Size: 10}, // no part ID: stored as "#0"
			{PartID: "2", Filename: "logo.png", MimeType: "image/png", Size: 10, ContentID: "l", Inline: true},
		}
	})
	if _, err := e.store.IngestMessages(context.Background(), []MessageIn{msg}, baseDate); err != nil {
		t.Fatal(err)
	}
	e.srv.fetcher = &fakeFetcher{files: map[string][]byte{"r1/#0": []byte("%PDF-1.4 original bytes")}}

	init := obj(e.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18"})["result"])
	if obj(obj(init["capabilities"])["resources"]) == nil {
		t.Fatal("resources capability not advertised")
	}
	tpl := list(obj(e.rpc("resources/templates/list", map[string]any{})["result"])["resourceTemplates"])
	if len(tpl) != 1 || str(obj(tpl[0])["uriTemplate"]) != "gmail-attachment://{message_id}/{attachment_id}" {
		t.Fatalf("templates: %v", tpl)
	}
	res := list(obj(e.rpc("resources/list", map[string]any{})["result"])["resources"])
	if len(res) != 1 || str(obj(res[0])["uri"]) != "gmail-attachment://r1/%230" || str(obj(res[0])["name"]) != "whole.pdf" {
		t.Fatalf("resources/list (signature logo must be left out): %v", res)
	}
	read := obj(e.rpc("resources/read", map[string]any{"uri": "gmail-attachment://r1/%230"})["result"])
	c := obj(list(read["contents"])[0])
	if blob, _ := base64.StdEncoding.DecodeString(str(c["blob"])); string(blob) != "%PDF-1.4 original bytes" || str(c["mimeType"]) != "application/pdf" {
		t.Fatalf("resources/read: %v", read)
	}
	for uri, code := range map[string]int{"gmail-attachment://r1": rpcResourceNotFound, "file:///etc/passwd": rpcResourceNotFound, "gmail-attachment://r1/9": rpcInternalError} {
		if out := e.rpc("resources/read", map[string]any{"uri": uri}); num(obj(out["error"])["code"]) != code {
			t.Errorf("%s: %v", uri, out)
		}
	}
}

// ---------------------------------------------------------------------------
// Apps Script web app protocol

// fakeWebApp mimics an Apps Script web app deployment: POST /exec answers with a
// redirect to /echo, where the signed JSON result is fetched with GET.
type fakeWebApp struct {
	secret   string
	files    map[string][]byte
	tamper   bool
	html     bool
	requests atomic.Int32
	pending  sync.Map // echo key -> response
}

func (f *fakeWebApp) sign(parts ...string) string {
	m := hmac.New(sha256.New, []byte(f.secret))
	m.Write([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(m.Sum(nil))
}

func (f *fakeWebApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "POST" && r.URL.Path == "/exec":
		f.requests.Add(1)
		var req struct {
			TS           json.Number `json:"ts"`
			Nonce        string      `json:"nonce"`
			MessageID    string      `json:"messageId"`
			AttachmentID string      `json:"attachmentId"`
			MaxBytes     json.Number `json:"maxBytes"`
			Sig          string      `json:"sig"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var out map[string]any
		if f.sign(fetchRequestLabel, req.TS.String(), req.Nonce, req.MessageID, req.AttachmentID, req.MaxBytes.String()) != req.Sig {
			out = map[string]any{"ok": false, "error": map[string]any{"code": "unauthorized", "message": "invalid signature"}}
		} else if data, ok := f.files[req.MessageID+"/"+req.AttachmentID]; !ok {
			out = map[string]any{"ok": false, "error": map[string]any{"code": "not_found", "message": "x"}}
		} else {
			b64 := base64.StdEncoding.EncodeToString(data)
			sum := sha256.Sum256([]byte(b64))
			digest := hex.EncodeToString(sum[:])
			out = map[string]any{"ok": true, "v": 1, "nonce": req.Nonce, "size": len(data), "sha256": digest, "data": b64,
				"sig": f.sign(fetchResponseLabel, req.Nonce, strconv.Itoa(len(data)), digest)}
			if f.tamper {
				out["data"] = base64.StdEncoding.EncodeToString(append(data, '!'))
			}
		}
		key := randomHex(8)
		f.pending.Store(key, out)
		http.Redirect(w, r, "/echo?user_content_key="+key, http.StatusFound)
	case r.Method == "GET" && r.URL.Path == "/echo":
		if f.html {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<html>Script function not found: doPost</html>")
			return
		}
		out, _ := f.pending.LoadAndDelete(r.URL.Query().Get("user_content_key"))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(out)
	case r.URL.Path == "/signin":
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html>Sign in</html>")
	default:
		http.NotFound(w, r)
	}
}

func TestWebAppFetcher(t *testing.T) {
	const secret = "fetch-secret-0123456789abcdef0123456789abcdef"
	app := &fakeWebApp{secret: secret, files: map[string][]byte{"m1/2": []byte("hello attachment")}}
	ts := httptest.NewServer(app)
	defer ts.Close()
	f := newWebAppFetcher(ts.URL+"/exec", secret, 5*time.Second, time.Now)
	ctx := context.Background()

	data, err := f.Fetch(ctx, "m1", "2", 1<<20)
	if err != nil || string(data) != "hello attachment" {
		t.Fatalf("fetch: %q %v", data, err)
	}
	if _, err := f.Fetch(ctx, "m1", "3", 1<<20); err == nil || !strings.Contains(err.Error(), "no longer has") {
		t.Fatalf("not found: %v", err)
	}
	if _, err := f.Fetch(ctx, "m1", "2", 4); err == nil || !strings.Contains(err.Error(), "larger than the limit") {
		t.Fatalf("size limit: %v", err)
	}
	wrong := newWebAppFetcher(ts.URL+"/exec", strings.Repeat("x", 40), 5*time.Second, time.Now)
	if _, err := wrong.Fetch(ctx, "m1", "2", 1<<20); err == nil || !strings.Contains(err.Error(), "ATTACHMENT_FETCH_SECRET") {
		t.Fatalf("wrong secret: %v", err)
	}
	app.tamper = true
	if _, err := f.Fetch(ctx, "m1", "2", 1<<20); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("tampered response accepted: %v", err)
	}
	app.tamper, app.html = false, true
	if _, err := f.Fetch(ctx, "m1", "2", 1<<20); err == nil || !strings.Contains(err.Error(), "doPost") {
		t.Fatalf("html page: %v", err)
	}
	app.html = false

	// A deployment that is not "Anyone" redirects to Google's sign-in page.
	signin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://accounts.google.com/ServiceLogin?continue=x", http.StatusFound)
	}))
	defer signin.Close()
	g := newWebAppFetcher(signin.URL+"/exec", secret, 5*time.Second, time.Now)
	if _, err := g.Fetch(ctx, "m1", "2", 1<<20); err == nil || !strings.Contains(err.Error(), "Who has access: Anyone") {
		t.Fatalf("sign-in redirect: %v", err)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) }))
	defer slow.Close()
	h := newWebAppFetcher(slow.URL+"/exec", secret, 300*time.Millisecond, time.Now)
	if _, err := h.Fetch(ctx, "m1", "2", 1<<20); err == nil || !strings.Contains(err.Error(), "within") {
		t.Fatalf("timeout: %v", err)
	}
}

func TestGetAttachmentThroughWebApp(t *testing.T) {
	const secret = "fetch-secret-0123456789abcdef0123456789abcdef"
	app := &fakeWebApp{secret: secret, files: map[string][]byte{"w1/2": []byte("Reading log, week 4")}}
	ts := httptest.NewServer(app)
	defer ts.Close()
	env := newAttEnv(t, func(c *Config) { c.AttachmentFetchURL, c.AttachmentFetchSecret = ts.URL+"/exec", secret })
	m := sampleMessage("w1", func(m *MessageIn) {
		m.Attachments = []AttachmentIn{{PartID: "2", Filename: "log.txt", MimeType: "text/plain", Size: 19}}
	})
	env.api.expect(env.api.do("POST", "/v1/sync/message", testSyncSecret, m), 200)
	_, text, _ := env.mcp.getAttachment(map[string]any{"message_id": "w1", "attachment_id": "2"})
	if text != "Reading log, week 4" || app.requests.Load() != 1 {
		t.Fatalf("web app path: %q", text)
	}
	out, _, _ := env.mcp.call("mailbox_status", map[string]any{})
	if str(obj(out["attachments"])["retrieval"]) != "Apps Script web app" {
		t.Fatalf("mailbox_status: %v", out["attachments"])
	}
	// No relay endpoints in web app mode.
	env.api.expect(env.api.do("POST", "/v1/attachments/relay/att_x", testSyncSecret, map[string]any{"ok": false}), 404)
}

// ---------------------------------------------------------------------------
// Relay through the action poll

func TestAttachmentRelay(t *testing.T) {
	env := newAttEnv(t, nil)
	m := sampleMessage("q1", func(m *MessageIn) {
		m.Attachments = []AttachmentIn{{PartID: "2", Filename: "menu.txt", MimeType: "text/plain", Size: 12}}
	})
	env.api.expect(env.api.do("POST", "/v1/sync/message", testSyncSecret, m), 200)

	pollRequests := func() []any {
		r := env.api.expect(env.api.do("POST", "/v1/actions/poll", testSyncSecret, map[string]any{"limit": 5}), 200)
		return list(r.body["attachmentRequests"])
	}
	type result struct {
		text  string
		isErr bool
		raw   []map[string]any
	}
	call := func() chan result {
		ch := make(chan result, 1)
		go func() {
			blocks, isErr := env.mcp.callResult("get_attachment", map[string]any{"message_id": "q1", "attachment_id": "2"})
			r := result{isErr: isErr, raw: blocks}
			if len(blocks) > 1 {
				r.text = str(blocks[1]["text"])
			} else if len(blocks) == 1 {
				r.text = str(blocks[0]["text"])
			}
			ch <- r
		}()
		return ch
	}
	waitRequest := func() map[string]any {
		for i := 0; i < 100; i++ {
			if reqs := pollRequests(); len(reqs) > 0 {
				return obj(reqs[0])
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("no attachment request offered to Apps Script")
		return nil
	}

	// 1. Apps Script picks the request up with its poll and uploads the file.
	done := call()
	req := waitRequest()
	if str(req["messageId"]) != "q1" || str(req["attachmentId"]) != "2" || num(req["maxBytes"]) != 10<<20 {
		t.Fatalf("request: %v", req)
	}
	if len(pollRequests()) != 0 {
		t.Fatal("a claimed request must not be offered again right away")
	}
	up := map[string]any{"ok": true, "data": base64.StdEncoding.EncodeToString([]byte("Pizza Friday")), "size": 12}
	env.api.expect(env.api.do("POST", "/v1/attachments/relay/"+str(req["id"]), testSyncSecret, up), 200)
	if r := <-done; r.isErr || r.text != "Pizza Friday" {
		t.Fatalf("relayed result: %+v", r)
	}
	env.api.expect(env.api.do("POST", "/v1/attachments/relay/"+str(req["id"]), testSyncSecret, up), 404)

	// 2. Apps Script reports an error: the agent sees why.
	done = call()
	req = waitRequest()
	fail := map[string]any{"ok": false, "error": map[string]any{"code": "not_found", "message": "gone"}}
	env.api.expect(env.api.do("POST", "/v1/attachments/relay/"+str(req["id"]), testSyncSecret, fail), 200)
	if r := <-done; !r.isErr || !strings.Contains(r.text, "no longer has") {
		t.Fatalf("relayed error: %+v", r)
	}

	// 3. Nobody picks it up in time: the agent is told to retry; a file arriving
	// after everyone gave up is refused (dropped), not kept.
	env.srv.cfg.AttachmentFetchTimeout = 300 * time.Millisecond
	if r := <-call(); !r.isErr || !strings.Contains(r.text, "get_attachment again") {
		t.Fatalf("timeout: %+v", r)
	}
	req = waitRequest()
	env.api.expect(env.api.do("POST", "/v1/attachments/relay/"+str(req["id"]), testSyncSecret, up), 410)

	// Wrong secret, bad data.
	env.api.expect(env.api.do("POST", "/v1/attachments/relay/x", testAPISecret, up), 401)
	done = call()
	req = waitRequest()
	bad := map[string]any{"ok": true, "data": "!!!", "size": 3}
	env.api.expect(env.api.do("POST", "/v1/attachments/relay/"+str(req["id"]), testSyncSecret, bad), 400)
	<-done
}

func TestAttachmentConfig(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{"SYNC_SECRET": testSyncSecret, "API_SECRET": testAPISecret}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	c, err := loadConfig(env())
	if err != nil || c.AttachmentMaxBytes != 10<<20 || c.AttachmentFetchURL != "" || c.AttachmentFetchTimeout != 40*time.Second {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	good := "https://script.google.com/macros/s/AKfycbx123/exec"
	c, err = loadConfig(env("ATTACHMENT_FETCH_URL", good, "ATTACHMENT_FETCH_SECRET", strings.Repeat("f", 40), "ATTACHMENT_MAX_BYTES", "5242880"))
	if err != nil || c.AttachmentFetchURL != good || c.AttachmentMaxBytes != 5<<20 {
		t.Fatalf("configured: %+v %v", c, err)
	}
	if _, err := loadConfig(env("ATTACHMENT_FETCH_URL", "https://script.google.com/a/macros/school.org/s/AKfyc/exec", "ATTACHMENT_FETCH_SECRET", strings.Repeat("f", 40))); err != nil {
		t.Errorf("Workspace-style URL refused: %v", err)
	}
	for _, bad := range [][]string{
		{"ATTACHMENT_FETCH_URL", good}, // no secret
		{"ATTACHMENT_FETCH_URL", good, "ATTACHMENT_FETCH_SECRET", testSyncSecret},
		{"ATTACHMENT_FETCH_URL", "https://evil.example.com/exec", "ATTACHMENT_FETCH_SECRET", strings.Repeat("f", 40)},
		{"ATTACHMENT_FETCH_URL", "https://script.google.com/macros/s/x/dev", "ATTACHMENT_FETCH_SECRET", strings.Repeat("f", 40)},
		{"ATTACHMENT_FETCH_URL", "http://script.google.com/macros/s/x/exec", "ATTACHMENT_FETCH_SECRET", strings.Repeat("f", 40)},
		{"ATTACHMENT_MAX_BYTES", "999999999"},
		{"ATTACHMENT_FETCH_TIMEOUT_SECONDS", "1"},
	} {
		if _, err := loadConfig(env(bad...)); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}
