package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	_ "image/gif" // registers GIF decoding for image.Decode
	_ "image/png" // registers PNG decoding for image.Decode
)

// Attachment conversion turns a downloaded file into text or images a model can
// read, entirely in memory. PDFs go through poppler's pdftotext/pdftoppm, which read
// the file on stdin and write to stdout, so nothing touches the disk. Those parse
// untrusted files in C, so they run inside bubblewrap (no network, no view of the
// home directory, database or secrets) whenever bwrap works on this machine.
// Word files, text and images are handled in Go.

const (
	maxConvertedText  = 4 << 20  // text kept from any one file
	maxConverterOut   = 16 << 20 // stdout of a converter process
	maxDocxXML        = 64 << 20 // decompressed word/document.xml (zip bomb guard)
	maxImagePixels    = 20_000_000
	maxImageSide      = 1568 // long side of images sent to the model
	passImageBytes    = 1 << 20
	maxPassImageBytes = 4 << 20 // formats Go cannot resize (WebP) are sent only up to this size
	maxRenderedPages  = 3       // pages without text rendered as images per call
)

type convertedImage struct {
	MimeType string
	Data     []byte
	Label    string
}

type converted struct {
	Kind   string // pdf, docx, text, image, unsupported
	Text   string
	Pages  int
	Images []convertedImage
	Notes  []string
}

type converter struct {
	log      *slog.Logger
	once     sync.Once
	bwrap    string // bwrap path when the sandbox works, else ""
	disabled bool   // tests: never use the sandbox
}

// sandboxArgs confines a converter: fresh namespaces (no network), read-only /usr and
// font configuration, empty /tmp, no capabilities, no environment.
func sandboxArgs(prog string, args []string) []string {
	out := []string{
		"--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL",
		"--ro-bind", "/usr", "/usr", "--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin",
		"--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64",
		"--ro-bind-try", "/etc/ld.so.cache", "/etc/ld.so.cache", "--ro-bind-try", "/etc/fonts", "/etc/fonts",
		"--ro-bind-try", "/var/cache/fontconfig", "/var/cache/fontconfig",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		"--clearenv", "--setenv", "PATH", "/usr/bin", "--setenv", "LANG", "C.UTF-8", "--setenv", "HOME", "/tmp",
		"--", prog,
	}
	return append(out, args...)
}

func (c *converter) sandbox() string {
	c.once.Do(func() {
		if c.disabled {
			return
		}
		bw, err := exec.LookPath("bwrap")
		if err != nil {
			c.log.Warn("attachments: bwrap not found; PDF converters run without a sandbox (install bubblewrap)")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, bw, sandboxArgs("/usr/bin/true", nil)...).CombinedOutput(); err != nil {
			c.log.Warn("attachments: bwrap does not work here; PDF converters run without a sandbox",
				"err", err.Error(), "output", truncateUTF8(strings.TrimSpace(string(out)), 200))
			return
		}
		c.bwrap = bw
		c.log.Info("attachments: PDF converters run in a bwrap sandbox")
	})
	return c.bwrap
}

var errConverterMissing = errors.New("converter not installed")

// run feeds input to a converter on stdin and returns its stdout.
func (c *converter) run(ctx context.Context, input []byte, prog string, args ...string) ([]byte, error) {
	path, err := exec.LookPath(prog)
	if err != nil {
		return nil, errConverterMissing
	}
	var cmd *exec.Cmd
	if bw := c.sandbox(); bw != "" {
		cmd = exec.CommandContext(ctx, bw, sandboxArgs(path, args)...)
	} else {
		cmd = exec.CommandContext(ctx, path, args...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	}
	cmd.Stdin = bytes.NewReader(input)
	cmd.WaitDelay = 2 * time.Second // do not hang on pipes a killed process left open
	stdout := &limitedBuffer{max: maxConverterOut}
	stderr := &limitedBuffer{max: 4 << 10, quiet: true}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// If a hostile file blows up memory, the kernel should kill the converter, not
	// the backend (both share the service's memory limit).
	_ = os.WriteFile(filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid), "oom_score_adj"), []byte("1000"), 0)
	err = cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case stdout.overflow:
		return nil, errors.New("converter output too large")
	case err != nil:
		return nil, fmt.Errorf("%s: %s", prog, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// limitedBuffer keeps at most max bytes. Past that it fails the write (stopping the
// converter) or, when quiet, drops the rest. It has no ReadFrom on purpose: io.Copy
// would use that and bypass the limit.
type limitedBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
	quiet    bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		b.overflow = true
		if b.quiet {
			b.buf.Write(p[:max(0, b.max-b.buf.Len())])
			return len(p), nil
		}
		return 0, errors.New("output limit exceeded")
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *limitedBuffer) String() string { return b.buf.String() }

// convert works out what the file really is from its bytes (the declared type is
// only a hint) and extracts what a model can read.
func (c *converter) convert(ctx context.Context, data []byte, filename, mimeType string) (*converted, error) {
	switch kind := sniffKind(data, filename, mimeType); kind {
	case "pdf":
		return c.convertPDF(ctx, data)
	case "docx":
		text, err := docxText(data)
		if err != nil {
			return &converted{Kind: "unsupported", Notes: []string{"This Word file could not be read: " + err.Error()}}, nil
		}
		return &converted{Kind: "docx", Text: text}, nil
	case "text":
		text := decodeText(data)
		if strings.Contains(mimeType, "html") || strings.HasSuffix(strings.ToLower(filename), ".html") || strings.HasSuffix(strings.ToLower(filename), ".htm") {
			text = htmlToText(text)
		}
		return &converted{Kind: "text", Text: strings.TrimRight(text, " \t\n")}, nil
	case "png", "jpeg", "gif", "webp":
		img, note := prepareImage(data, kind)
		out := &converted{Kind: "image"}
		if img != nil {
			out.Images = []convertedImage{*img}
		}
		if note != "" {
			out.Notes = append(out.Notes, note)
		}
		return out, nil
	default:
		return &converted{Kind: "unsupported", Notes: []string{unsupportedNote(data, kind)}}, nil
	}
}

func unsupportedNote(data []byte, kind string) string {
	what := map[string]string{
		"xlsx": "an Excel spreadsheet", "pptx": "a PowerPoint presentation", "zip": "a ZIP archive",
		"heic": "a HEIC photo", "tiff": "a TIFF image", "bmp": "a BMP image", "ole": "an old-style Office file (.doc/.xls/.ppt)",
	}[kind]
	if what == "" {
		what = "a file type"
	}
	return "This is " + what + " that cannot be converted to text or an image here. Supported: PDF, Word (.docx), " +
		"text and CSV files, and PNG/JPEG/GIF/WebP images. Open it in Gmail instead."
}

func sniffKind(data []byte, filename, mimeType string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	head := data[:min(len(data), 1024)]
	switch {
	case bytes.Contains(head, []byte("%PDF-")):
		return "pdf"
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return "jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "gif"
	case len(data) > 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "webp"
	case len(data) > 12 && string(data[4:8]) == "ftyp" && strings.Contains("heic heix mif1 msf1 heim heis avif", string(data[8:12])):
		return "heic"
	case bytes.HasPrefix(data, []byte("II*\x00")), bytes.HasPrefix(data, []byte("MM\x00*")):
		return "tiff"
	case bytes.HasPrefix(data, []byte("BM")) && ext == ".bmp":
		return "bmp"
	case bytes.HasPrefix(data, []byte{0xD0, 0xCF, 0x11, 0xE0}):
		return "ole"
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		return zipKind(data)
	}
	if looksLikeText(data) && (textExtensions[ext] || ext == "" || mimeType == "application/octet-stream" ||
		strings.HasPrefix(mimeType, "text/") || strings.Contains(mimeType, "json") || strings.Contains(mimeType, "xml") || strings.Contains(mimeType, "csv")) {
		return "text"
	}
	return "other"
}

var textExtensions = map[string]bool{
	".txt": true, ".csv": true, ".tsv": true, ".md": true, ".json": true, ".xml": true, ".ics": true, ".vcf": true,
	".html": true, ".htm": true, ".log": true, ".srt": true, ".vtt": true,
}

func zipKind(data []byte) string {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "zip"
	}
	for _, f := range zr.File {
		switch f.Name {
		case "word/document.xml":
			return "docx"
		case "xl/workbook.xml":
			return "xlsx"
		case "ppt/presentation.xml":
			return "pptx"
		}
	}
	return "zip"
}

// looksLikeText accepts UTF-16 with a byte order mark, or bytes without NULs and
// with few control characters in the first 8 KiB.
func looksLikeText(data []byte) bool {
	if bytes.HasPrefix(data, []byte{0xFF, 0xFE}) || bytes.HasPrefix(data, []byte{0xFE, 0xFF}) {
		return true
	}
	head := data[:min(len(data), 8192)]
	ctl := 0
	for _, b := range head {
		switch {
		case b == 0:
			return false
		case b < 0x20 && b != '\n' && b != '\r' && b != '\t' && b != '\f':
			ctl++
		}
	}
	return ctl*100 <= len(head)
}

// decodeText reads UTF-8 (with or without BOM) or UTF-16 with a BOM; anything else
// is taken as Windows-1252/Latin-1, the usual encoding of older CSV exports.
func decodeText(data []byte) string {
	var s string
	switch {
	case bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}):
		s = string(data[3:])
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}), bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		le := data[0] == 0xFF
		u := make([]uint16, 0, len(data)/2)
		for i := 2; i+1 < len(data); i += 2 {
			if le {
				u = append(u, uint16(data[i])|uint16(data[i+1])<<8)
			} else {
				u = append(u, uint16(data[i])<<8|uint16(data[i+1]))
			}
		}
		s = string(utf16.Decode(u))
	case utf8.Valid(data):
		s = string(data)
	default:
		r := make([]rune, len(data))
		for i, b := range data {
			r[i] = rune(b)
		}
		s = string(r)
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\x00", "")
	return strings.ToValidUTF8(s, "�")
}

// ---------------------------------------------------------------------------
// PDF

func (c *converter) convertPDF(ctx context.Context, data []byte) (*converted, error) {
	out, err := c.run(ctx, data, "pdftotext", "-q", "-enc", "UTF-8", "-eol", "unix", "-", "-")
	switch {
	case errors.Is(err, errConverterMissing):
		return &converted{Kind: "unsupported", Notes: []string{"PDF text extraction is not installed on the server (package poppler-utils)."}}, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		msg := "This PDF could not be read; it may be damaged."
		if strings.Contains(strings.ToLower(err.Error()), "password") || strings.Contains(err.Error(), "ncrypt") {
			msg = "This PDF is password-protected, so its text cannot be extracted."
		}
		return &converted{Kind: "unsupported", Notes: []string{msg}}, nil
	}
	// pdftotext ends every page with a form feed.
	pages := strings.Split(strings.TrimSuffix(string(out), "\f"), "\f")
	res := &converted{Kind: "pdf", Pages: len(pages)}
	var b strings.Builder
	var empty []int
	for i, p := range pages {
		p = strings.TrimSpace(p)
		if len(strings.Join(strings.Fields(p), "")) < 10 {
			empty = append(empty, i+1)
		}
		if len(pages) > 1 {
			fmt.Fprintf(&b, "--- Page %d ---\n", i+1)
		}
		b.WriteString(p)
		b.WriteString("\n\n")
		if b.Len() > maxConvertedText {
			res.Notes = append(res.Notes, fmt.Sprintf("Only the first %d pages are included; the PDF has more text than the server keeps.", i+1))
			break
		}
	}
	res.Text = strings.TrimSpace(b.String())
	if len(empty) == 0 {
		return res, nil
	}
	if len(empty) == len(pages) {
		res.Text = ""
	}
	// Pages without a text layer are scans or pictures: send them as images.
	var rendered []string
	for _, n := range empty[:min(len(empty), maxRenderedPages)] {
		img, err := c.run(ctx, data, "pdftoppm", "-f", strconv.Itoa(n), "-l", strconv.Itoa(n), "-singlefile",
			"-scale-to", strconv.Itoa(maxImageSide), "-jpeg", "-jpegopt", "quality=80", "-")
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil || len(img) == 0 {
			break
		}
		res.Images = append(res.Images, convertedImage{MimeType: "image/jpeg", Data: img, Label: "page " + strconv.Itoa(n)})
		rendered = append(rendered, strconv.Itoa(n))
	}
	note := fmt.Sprintf("%d of %d pages have no text layer (scanned or pictures); text recognition (OCR) is not available.", len(empty), len(pages))
	if len(rendered) > 0 {
		note += " Page(s) " + strings.Join(rendered, ", ") + " are attached as images."
		if len(empty) > len(rendered) {
			note += fmt.Sprintf(" %d more such page(s) were not rendered.", len(empty)-len(rendered))
		}
	}
	res.Notes = append(res.Notes, note)
	return res, nil
}

// ---------------------------------------------------------------------------
// Word (.docx)

// docxText reads the main document part: paragraphs as lines, table cells separated
// by tabs. Headers, footers and comments are left out.
func docxText(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", errors.New("not a valid .docx (zip) file")
	}
	var doc *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			doc = f
			break
		}
	}
	if doc == nil {
		return "", errors.New("word/document.xml is missing")
	}
	if doc.UncompressedSize64 > maxDocxXML {
		return "", errors.New("document is too large")
	}
	rc, err := doc.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	dec := xml.NewDecoder(io.LimitReader(rc, maxDocxXML))
	var b strings.Builder
	inText, cells := false, 0
	for b.Len() < maxConvertedText {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			if b.Len() > 0 {
				break // keep what was readable
			}
			return "", errors.New("document.xml is damaged")
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				b.WriteByte('\t')
			case "br", "cr":
				b.WriteByte('\n')
			case "tc":
				cells++
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				if cells > 0 {
					b.WriteByte(' ') // paragraphs inside a table cell stay on the row's line
				} else {
					b.WriteByte('\n')
				}
			case "tc":
				cells--
				b.WriteByte('\t')
			case "tr":
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				b.Write(t)
			}
		}
	}
	text := strings.ReplaceAll(strings.ReplaceAll(b.String(), " \t", "\t"), "\t\n", "\n")
	return strings.TrimSpace(reBlankLines.ReplaceAllString(text, "\n\n")), nil
}

// ---------------------------------------------------------------------------
// Images

// prepareImage passes small images through and scales big ones down to a JPEG whose
// long side is maxImageSide, so a phone photo does not become megabytes of base64.
func prepareImage(data []byte, kind string) (*convertedImage, string) {
	mimeType := "image/" + kind
	if kind == "webp" {
		if len(data) > maxPassImageBytes {
			return nil, "This WebP image is too large to send and the server cannot resize WebP."
		}
		return &convertedImage{MimeType: mimeType, Data: data}, ""
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "This image could not be read; it may be damaged."
	}
	if cfg.Width*cfg.Height > maxImagePixels {
		return nil, fmt.Sprintf("This image is %dx%d pixels, too large to process here.", cfg.Width, cfg.Height)
	}
	if len(data) <= passImageBytes && max(cfg.Width, cfg.Height) <= maxImageSide*5/4 {
		return &convertedImage{MimeType: mimeType, Data: data}, ""
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "This image could not be read; it may be damaged."
	}
	small := downscale(img, maxImageSide)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, small, &jpeg.Options{Quality: 85}); err != nil {
		return nil, "This image could not be converted."
	}
	b := small.Bounds()
	return &convertedImage{MimeType: "image/jpeg", Data: buf.Bytes()},
		fmt.Sprintf("Scaled down from %dx%d to %dx%d pixels.", cfg.Width, cfg.Height, b.Dx(), b.Dy())
}

// downscale shrinks img so its long side is at most side pixels, averaging each
// block of source pixels (box filter). Transparent areas become white.
func downscale(img image.Image, side int) image.Image {
	sb := img.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dw, dh := sw, sh
	switch {
	case max(sw, sh) <= side:
	case sw >= sh:
		dw, dh = side, max(1, sh*side/sw)
	default:
		dw, dh = max(1, sw*side/sh), side
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	at := func(x, y int) color.RGBA64 { return color.RGBA64Model.Convert(img.At(x, y)).(color.RGBA64) }
	if fast, ok := img.(image.RGBA64Image); ok {
		at = fast.RGBA64At
	}
	for dy := 0; dy < dh; dy++ {
		y0, y1 := sb.Min.Y+dy*sh/dh, sb.Min.Y+(dy+1)*sh/dh
		for dx := 0; dx < dw; dx++ {
			x0, x1 := sb.Min.X+dx*sw/dw, sb.Min.X+(dx+1)*sw/dw
			var r, g, b, a, n uint64
			for y := y0; y < max(y1, y0+1); y++ {
				for x := x0; x < max(x1, x0+1); x++ {
					c := at(x, y)
					r, g, b, a, n = r+uint64(c.R), g+uint64(c.G), b+uint64(c.B), a+uint64(c.A), n+1
				}
			}
			// Premultiplied colour over a white background.
			white := (n*0xffff - a)
			dst.SetRGBA(dx, dy, color.RGBA{
				R: uint8((r + white) / n >> 8), G: uint8((g + white) / n >> 8), B: uint8((b + white) / n >> 8), A: 0xff,
			})
		}
	}
	return dst
}
