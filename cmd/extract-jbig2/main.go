// Command extract-jbig2 walks a PDF and dumps every
// /Filter /JBIG2Decode image XObject's stream bytes (plus any
// referenced /JBIG2Globals) as separate .jb2 files. Output is the
// exact bytes a JBIG2 decoder receives from a PDF reader's
// /JBIG2Decode filter - suitable as gobig2 test fixtures.
//
// Usage:
//
//	extract-jbig2 [-out DIR] [-force] PDF [PDF ...]
//
// For each input PDF "foo.pdf" with image XObject N (optional
// globals G), writes:
//
//	DIR/foo-objN.jb2          # image stream
//	DIR/foo-objN.globals.jb2  # globals stream (if /JBIG2Globals)
//
// Plus "DIR/foo-objN.txt" sidecar with source basename, object
// number, /Width x /Height.
//
// # Decoding the output
//
// Extracted `.jb2` files are PDF-embedded streams - headerless
// segment sequences without the T.88 Annex E file-header magic.
// They will NOT decode through [image.Decode] /
// [image.DecodeConfig] (those match on magic). Use the gobig2
// CLI (auto-detects via NewDecoderEmbedded fallback) or
// [gobig2.NewDecoderEmbedded] directly. Globals sidecars pair
// via [gobig2.NewDecoderEmbeddedWithGlobals] or
// `--globals path-to-globals.jb2`.
//
// Batch: PDFs from different dirs sharing a basename (e.g.
// `a/foo.pdf` and `b/foo.pdf` both -> `foo-obj4.jb2`) fail on
// second collision. Pass `-force` to overwrite, or pick distinct
// `-out` per input.
//
// # Scope
//
// Handles classic PDF 1.4-1.5: xref tables, indirect objects,
// simple stream dicts. Literal `/Length N` and indirect
// `/Length N 0 R` both honored; indirect resolves via parsed
// object map. Does NOT handle:
//
//   - Cross-reference streams (PDF 1.5+ /Root /Type /XRef)
//   - Object streams (compressed objects)
//   - Encrypted PDFs
//   - Filter chains other than bare /JBIG2Decode (e.g.
//     /Filter [/FlateDecode /JBIG2Decode] skipped)
//   - Linearized PDFs with stream xref
//
// Out-of-scope PDFs skipped with "skipped: <reason>" on stderr.
// Built for batch extraction over a known-shape corpus (scanner
// output etc).
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// pdfDoc holds parsed PDF state enough to resolve indirect refs
// to bytes. One map: objNum -> raw body (between "N 0 obj" and
// "endobj").
type pdfDoc struct {
	data    []byte         // entire PDF bytes
	objects map[int][]byte // objNum -> raw object body (dict + stream)
}

// defaultMaxPDFBytes caps per-PDF input by default. extractOne
// os.ReadFiles the whole PDF up front; without this, a batched
// dir with an unexpectedly large file allocates full size before
// layout-rejection runs. 256 MiB matches gobig2.MaxInputBytes,
// well above realistic fixture corpora (scanned multi-page docs
// 10-50 MB).
const defaultMaxPDFBytes = 256 * 1024 * 1024

func main() {
	var outDir string
	var force bool
	var maxPDFBytes int64
	flag.StringVar(&outDir, "out", "extracted", "output directory")
	flag.BoolVar(&force, "force", false,
		"overwrite existing extracted files (default: fail-on-exists so batched PDFs with colliding basenames don't silently overwrite each other)")
	flag.Int64Var(&maxPDFBytes, "max-pdf-bytes", defaultMaxPDFBytes,
		"reject input PDFs larger than this many bytes (default 256 MiB; the extractor pre-reads the entire PDF, so this caps batch-tool memory use against unexpectedly large inputs). 0 disables the check.")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: extract-jbig2 [-out DIR] [-force] [-max-pdf-bytes N] PDF [PDF ...]")
		fmt.Fprintln(os.Stderr, "note: output is PDF-embedded (no JBIG2 file header); decode via the gobig2 CLI or gobig2.NewDecoderEmbedded, not image.Decode.")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "extract-jbig2: mkdir %s: %v\n", outDir, err)
		os.Exit(1)
	}

	totalImages := 0
	var failures []string
	for _, pdfPath := range flag.Args() {
		count, err := extractOne(pdfPath, outDir, force, maxPDFBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "extract-jbig2: %s: %v\n", pdfPath, err)
			failures = append(failures, pdfPath)
			continue
		}
		totalImages += count
	}
	fmt.Fprintf(os.Stderr, "extract-jbig2: %d image(s) extracted, %d PDF(s) failed\n",
		totalImages, len(failures))
	if len(failures) > 0 {
		os.Exit(1)
	}
}

func extractOne(pdfPath, outDir string, force bool, maxPDFBytes int64) (int, error) {
	// Stat pre-check: fail fast on huge PDF before alloc.
	// maxPDFBytes <= 0 disables.
	if maxPDFBytes > 0 {
		fi, err := os.Stat(pdfPath)
		if err != nil {
			return 0, err
		}
		if fi.Size() > maxPDFBytes {
			return 0, fmt.Errorf("%s: PDF size %d bytes exceeds --max-pdf-bytes %d",
				pdfPath, fi.Size(), maxPDFBytes)
		}
	}
	data, err := os.ReadFile(pdfPath) //nolint:gosec // path is user-supplied by design
	if err != nil {
		return 0, err
	}
	doc, err := parsePDF(data)
	if err != nil {
		return 0, fmt.Errorf("parse: %w", err)
	}
	base := strings.TrimSuffix(filepath.Base(pdfPath), filepath.Ext(pdfPath))

	// writeOnce writes payload to outDir/name, refusing to
	// overwrite unless --force. Without fail-on-exists, batched
	// extraction over basename-sharing PDFs silently overwrites.
	//
	// Default path (force=false) uses O_CREATE|O_EXCL so check+
	// write is atomic. Stat+WriteFile leaves a TOCTOU window.
	writeOnce := func(name string, payload []byte) error {
		// gosec G703 can't see that `name` is filepath.Base
		// (pdfPath) + fixed suffix; safe by construction.
		path := filepath.Join(outDir, name)
		if force {
			return os.WriteFile(path, payload, 0o600) //nolint:gosec
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s: refusing to overwrite (pass --force or pick a different --out): "+
					"basename collision while processing %s", path, pdfPath)
			}
			return err
		}
		if _, werr := f.Write(payload); werr != nil {
			_ = f.Close()
			return werr
		}
		return f.Close()
	}

	count := 0
	for objNum, body := range doc.objects {
		if !isJBIG2Image(body) {
			continue
		}
		stream, err := extractStream(body, doc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipped: %s obj %d: %v\n", pdfPath, objNum, err)
			continue
		}
		if err := writeOnce(fmt.Sprintf("%s-obj%d.jb2", base, objNum), stream); err != nil {
			return count, err
		}

		// Globals are referenced via /DecodeParms <</JBIG2Globals N 0 R>>.
		if globalsRef := findGlobalsRef(body); globalsRef > 0 {
			gBody, ok := doc.objects[globalsRef]
			if !ok {
				fmt.Fprintf(os.Stderr, "skipped globals: %s obj %d -> %d (not in xref)\n",
					pdfPath, objNum, globalsRef)
			} else if gStream, err := extractStream(gBody, doc); err != nil {
				fmt.Fprintf(os.Stderr, "skipped globals: %s obj %d -> %d: %v\n",
					pdfPath, objNum, globalsRef, err)
			} else if err := writeOnce(
				fmt.Sprintf("%s-obj%d.globals.jb2", base, objNum), gStream,
			); err != nil {
				return count, err
			}
		}

		// Provenance sidecar. Basename only - else committed
		// sidecar bakes an absolute path that looks weird on
		// fresh clone.
		w, h := extractWidthHeight(body)
		sidecar := fmt.Sprintf(
			"source:    %s\nobject:    %d\ndimensions: %dx%d\nformat:    PDF-embedded JBIG2 (headerless); decode via gobig2 CLI or gobig2.NewDecoderEmbedded\n",
			filepath.Base(pdfPath), objNum, w, h,
		)
		if err := writeOnce(
			fmt.Sprintf("%s-obj%d.txt", base, objNum), []byte(sidecar),
		); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// parsePDF reads xref + indirect objects. Permissive subset:
// classic xref table, no encryption, no object streams.
func parsePDF(data []byte) (*pdfDoc, error) {
	doc := &pdfDoc{data: data, objects: make(map[int][]byte)}

	// 1. Locate trailing xref: "startxref\n<offset>\n%%EOF".
	idx := bytes.LastIndex(data, []byte("startxref"))
	if idx < 0 {
		return nil, fmt.Errorf("startxref not found")
	}
	rest := data[idx+len("startxref"):]
	rest = bytes.TrimLeft(rest, " \t\r\n")
	end := bytes.IndexAny(rest, " \t\r\n")
	if end < 0 {
		return nil, fmt.Errorf("startxref offset not terminated")
	}
	xrefOffset, err := strconv.ParseInt(string(rest[:end]), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("startxref offset: %w", err)
	}
	if xrefOffset < 0 || xrefOffset >= int64(len(data)) {
		return nil, fmt.Errorf("startxref offset out of range")
	}

	// 2. Parse xref table at the offset.
	if !bytes.HasPrefix(data[xrefOffset:], []byte("xref")) {
		return nil, fmt.Errorf("xref stream (PDF 1.5+) not supported; expected 'xref' table at offset %d", xrefOffset)
	}
	if err := parseXrefTable(data, xrefOffset, doc); err != nil {
		return nil, fmt.Errorf("xref: %w", err)
	}
	return doc, nil
}

// parseXrefTable walks xref entries from offset, fills
// doc.objects. Entry format: "OOOOOOOOOO GGGGG n \n" (10-digit
// offset, 5-digit gen, 'n'=in-use or 'f'=free, 1-byte sep).
func parseXrefTable(data []byte, offset int64, doc *pdfDoc) error {
	r := bufio.NewReader(bytes.NewReader(data[offset:]))
	if _, err := r.ReadString('\n'); err != nil { // "xref\n"
		return err
	}
	for {
		header, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		header = strings.TrimSpace(header)
		if header == "" {
			continue
		}
		if header == "trailer" || strings.HasPrefix(header, "trailer") {
			break
		}
		// Section header: "FIRST COUNT"
		parts := strings.Fields(header)
		if len(parts) != 2 {
			return fmt.Errorf("bad xref section header: %q", header)
		}
		first, err := strconv.Atoi(parts[0])
		if err != nil {
			return fmt.Errorf("xref first: %w", err)
		}
		count, err := strconv.Atoi(parts[1])
		if err != nil {
			return fmt.Errorf("xref count: %w", err)
		}
		for i := 0; i < count; i++ {
			line, err := r.ReadString('\n')
			if err != nil {
				return err
			}
			fields := strings.Fields(line)
			if len(fields) < 3 {
				return fmt.Errorf("bad xref entry: %q", line)
			}
			if fields[2] != "n" {
				continue // 'f' = free entry
			}
			objOffset, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil {
				return fmt.Errorf("xref offset: %w", err)
			}
			objNum := first + i
			if body, ok := readObjectBody(data, objOffset); ok {
				doc.objects[objNum] = body
			}
		}
	}
	return nil
}

// objectHeaderRe anchors at xref offset, matches canonical
// indirect-object header `N G obj` (objnum + gen + `obj` +
// whitespace terminator). Whitespace tolerant inside header,
// explicit about what precedes body.
var objectHeaderRe = regexp.MustCompile(`^\s*(\d+)\s+(\d+)\s+obj[\s\r\n]`)

// readObjectBody extracts bytes between "N G obj" and "endobj"
// at the file offset. Returns body (incl. any "stream...
// endstream") + success flag.
//
// Validates object header at offset rather than scanning for
// first "obj" - naive scan matches "obj" inside other contexts
// (e.g. `/Subject (obj...)`) appearing before real header on
// malformed inputs.
func readObjectBody(data []byte, offset int64) ([]byte, bool) {
	if offset < 0 || offset >= int64(len(data)) {
		return nil, false
	}
	tail := data[offset:]
	header := objectHeaderRe.FindIndex(tail)
	if header == nil {
		return nil, false
	}
	body := tail[header[1]:]
	end := bytes.Index(body, []byte("endobj"))
	if end < 0 {
		return nil, false
	}
	return body[:end], true
}

// isJBIG2Image returns true iff the object body declares
// /Subtype /Image AND /Filter /JBIG2Decode (bare; no filter
// chain).
func isJBIG2Image(body []byte) bool {
	// Restrict to the dict portion (before 'stream').
	dict := body
	if i := bytes.Index(body, []byte("stream")); i >= 0 {
		dict = body[:i]
	}
	if !bytes.Contains(dict, []byte("/Subtype /Image")) &&
		!bytes.Contains(dict, []byte("/Subtype/Image")) {
		return false
	}
	// Exclude filter chains: "/Filter [/FlateDecode /JBIG2Decode]" etc.
	if bytes.Contains(dict, []byte("/Filter [")) || bytes.Contains(dict, []byte("/Filter[")) {
		return false
	}
	return bytes.Contains(dict, []byte("/Filter /JBIG2Decode")) ||
		bytes.Contains(dict, []byte("/Filter/JBIG2Decode"))
}

// findGlobalsRef returns the object number from
// /DecodeParms <</JBIG2Globals N 0 R>> or 0 if absent.
var globalsRefRe = regexp.MustCompile(`/JBIG2Globals\s+(\d+)\s+\d+\s+R`)

func findGlobalsRef(body []byte) int {
	dict := body
	if i := bytes.Index(body, []byte("stream")); i >= 0 {
		dict = body[:i]
	}
	m := globalsRefRe.FindSubmatch(dict)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0
	}
	return n
}

// extractStream pulls bytes between "stream\n" and "\nendstream".
// Honors dict's /Length as authority (trailing newline varies;
// /Length is spec-mandated truth).
var (
	lengthRe         = regexp.MustCompile(`/Length\s+(\d+)(?:\s|>)`)
	lengthIndirectRe = regexp.MustCompile(`/Length\s+(\d+)\s+\d+\s+R`)
	indirectIntRe    = regexp.MustCompile(`^\s*(\d+)\s*$`)
)

// extractStream pulls stream bytes from a PDF object body.
// Honors literal `/Length N` and indirect `/Length N 0 R` -
// indirect resolves via doc.objects so real-world PDFs emitting
// length as a separate object aren't skipped.
func extractStream(body []byte, doc *pdfDoc) ([]byte, error) {
	streamIdx := bytes.Index(body, []byte("stream"))
	if streamIdx < 0 {
		return nil, fmt.Errorf("no stream keyword")
	}
	// Skip "stream" + line terminator. Per spec data starts
	// after single CR LF or LF.
	tail := body[streamIdx+len("stream"):]
	if len(tail) > 0 && tail[0] == '\r' {
		tail = tail[1:]
	}
	if len(tail) > 0 && tail[0] == '\n' {
		tail = tail[1:]
	}
	// Use /Length from the dict - literal first, then indirect.
	dict := body[:streamIdx]
	length, err := resolveLength(dict, doc)
	if err != nil {
		return nil, err
	}
	if length < 0 || length > len(tail) {
		return nil, fmt.Errorf("/Length %d exceeds remaining %d", length, len(tail))
	}
	return tail[:length], nil
}

// resolveLength returns stream length from dict. Tries indirect
// `/Length N 0 R` first - literal regex would otherwise match
// the leading objnum digit of an indirect ref (Go regexp has no
// negative lookahead for "digits not followed by int + R").
func resolveLength(dict []byte, doc *pdfDoc) (int, error) {
	if m := lengthIndirectRe.FindSubmatch(dict); m != nil {
		objNum, err := strconv.Atoi(string(m[1]))
		if err != nil {
			return 0, fmt.Errorf("bad indirect /Length object: %w", err)
		}
		if doc == nil {
			return 0, fmt.Errorf("indirect /Length %d 0 R: no document context to resolve", objNum)
		}
		lenBody, ok := doc.objects[objNum]
		if !ok {
			return 0, fmt.Errorf("indirect /Length %d 0 R: referenced object not in xref", objNum)
		}
		mm := indirectIntRe.FindSubmatch(lenBody)
		if mm == nil {
			return 0, fmt.Errorf("indirect /Length %d 0 R: body is not a bare integer", objNum)
		}
		length, err := strconv.Atoi(string(mm[1]))
		if err != nil {
			return 0, fmt.Errorf("indirect /Length %d 0 R body: %w", objNum, err)
		}
		return length, nil
	}
	if m := lengthRe.FindSubmatch(dict); m != nil {
		length, err := strconv.Atoi(string(m[1]))
		if err != nil {
			return 0, fmt.Errorf("bad /Length: %w", err)
		}
		return length, nil
	}
	return 0, fmt.Errorf("no /Length in dict")
}

// extractWidthHeight pulls /Width and /Height from the dict for
// the provenance sidecar. Best-effort; returns 0,0 on miss.
var (
	widthRe  = regexp.MustCompile(`/Width\s+(\d+)`)
	heightRe = regexp.MustCompile(`/Height\s+(\d+)`)
)

func extractWidthHeight(body []byte) (int, int) {
	dict := body
	if i := bytes.Index(body, []byte("stream")); i >= 0 {
		dict = body[:i]
	}
	w, h := 0, 0
	if m := widthRe.FindSubmatch(dict); m != nil {
		w, _ = strconv.Atoi(string(m[1]))
	}
	if m := heightRe.FindSubmatch(dict); m != nil {
		h, _ = strconv.Atoi(string(m[1]))
	}
	return w, h
}

// Compile-time keepalive: io used to keep linters honest as
// file grows.
var _ = io.EOF
