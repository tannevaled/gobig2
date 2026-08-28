package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tannevaled/gobig2"
)

// Synthesizing a minimal valid PDF inline lets extractor tests
// run hermetically without a PDF corpus in testdata/. Each
// builder writes correct xref offsets so parseXrefTable hits
// indirect objects.

// jbig2Magic marker confirms extraction pulled the right byte
// range. Real JBIG2 streams start with this (T.88 Annex E file
// header magic); PDF-embedded streams omit it - used here as a
// synthetic distinguishable payload.
var jbig2Magic = []byte{0x97, 0x4A, 0x42, 0x32, 0x0D, 0x0A, 0x1A, 0x0A}

// makeMinimalJBIG2PDF writes a tiny PDF 1.4 with one /JBIG2Decode
// image XObject (no globals). Simplest valid shape extractor
// handles; xref precomputed.
func makeMinimalJBIG2PDF(payload []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")

	type objRec struct {
		offset int
		body   string
	}
	objs := []objRec{
		{0, "<< /Type /Catalog /Pages 2 0 R >>"},
		{0, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"},
		{0, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] /Resources << /XObject << /Im 4 0 R >> >> >>"},
		{0, ""}, // image XObject body filled below
	}
	imgDict := fmt.Sprintf(
		"<< /Length %d /Type /XObject /Subtype /Image /Width 100 /Height 100 /ColorSpace /DeviceGray /Filter /JBIG2Decode /BitsPerComponent 1 >>",
		len(payload),
	)
	objs[3].body = imgDict + "\nstream\n" + string(payload) + "\nendstream"

	for i := range objs {
		objs[i].offset = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, objs[i].body)
	}
	xrefOffset := buf.Len()
	buf.WriteString("xref\n")
	fmt.Fprintf(&buf, "0 %d\n", len(objs)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, o := range objs {
		fmt.Fprintf(&buf, "%010d 00000 n \n", o.offset)
	}
	buf.WriteString("trailer\n<< /Size 5 /Root 1 0 R >>\nstartxref\n")
	fmt.Fprintf(&buf, "%d\n", xrefOffset)
	buf.WriteString("%%EOF\n")
	return buf.Bytes()
}

// TestExtractSimple confirms the extractor finds and writes the
// JBIG2 stream from a minimal single-image PDF.
func TestExtractSimple(t *testing.T) {
	pdfPath := filepath.Join(t.TempDir(), "simple.pdf")
	outDir := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatalf("mkdir outdir: %v", err)
	}
	payload := append(append([]byte{}, jbig2Magic...), []byte("synthetic-payload")...)
	if err := os.WriteFile(pdfPath, makeMinimalJBIG2PDF(payload), 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}

	n, err := extractOne(pdfPath, outDir, false, 0)
	if err != nil {
		t.Fatalf("extractOne: %v", err)
	}
	if n != 1 {
		t.Fatalf("extracted %d streams, want 1", n)
	}

	// The image XObject is object 4 in our minimal layout.
	base := strings.TrimSuffix(filepath.Base(pdfPath), ".pdf")
	wantJB2 := filepath.Join(outDir, base+"-obj4.jb2")
	wantTxt := filepath.Join(outDir, base+"-obj4.txt")
	got, err := os.ReadFile(wantJB2)
	if err != nil {
		t.Fatalf("read extracted .jb2: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("extracted payload mismatch:\n got %q\nwant %q", got, payload)
	}
	if _, err := os.Stat(wantTxt); err != nil {
		t.Errorf("provenance sidecar missing: %v", err)
	}
}

// TestExtractSidecarContents confirms provenance .txt has source
// basename, object number, dims (not full path - else commits
// bake the share path).
func TestExtractSidecarContents(t *testing.T) {
	pdfPath := filepath.Join(t.TempDir(), "withdims.pdf")
	outDir := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatalf("mkdir outdir: %v", err)
	}
	if err := os.WriteFile(pdfPath, makeMinimalJBIG2PDF([]byte("payload")), 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	if _, err := extractOne(pdfPath, outDir, false, 0); err != nil {
		t.Fatalf("extractOne: %v", err)
	}
	sidecar := filepath.Join(outDir, "withdims-obj4.txt")
	data, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		"source:    withdims.pdf",
		"object:    4",
		"dimensions: 100x100",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sidecar missing %q; got:\n%s", want, body)
		}
	}
	if strings.Contains(body, "/tmp/") || strings.Contains(body, t.TempDir()) {
		t.Errorf("sidecar leaked absolute path:\n%s", body)
	}
}

// TestExtractNoXref rejects a buffer with no startxref marker.
// Extractor errors "startxref not found" rather than crashing.
func TestExtractNoXref(t *testing.T) {
	pdfPath := filepath.Join(t.TempDir(), "broken.pdf")
	outDir := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(pdfPath, []byte("%PDF-1.4\nno xref here\n"), 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	_, err := extractOne(pdfPath, outDir, false, 0)
	if err == nil {
		t.Fatal("extractOne accepted no-xref input")
	}
	if !strings.Contains(err.Error(), "startxref") {
		t.Errorf("err = %v, want 'startxref' mention", err)
	}
}

// TestExtractXrefStreamUnsupported: PDF 1.5+ xref-stream inputs
// surface as clear error (no crash). Out of documented scope.
func TestExtractXrefStreamUnsupported(t *testing.T) {
	pdfPath := filepath.Join(t.TempDir(), "xrefstream.pdf")
	outDir := filepath.Join(t.TempDir(), "out")
	// PDF whose startxref points at a non-xref-table byte.
	body := []byte("%PDF-1.5\n1 0 obj\n<< /Type /XRef >>\nstream\nfake stream bytes\nendstream\nendobj\nstartxref\n9\n%%EOF\n")
	if err := os.WriteFile(pdfPath, body, 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	_, err := extractOne(pdfPath, outDir, false, 0)
	if err == nil {
		t.Fatal("extractOne accepted xref-stream PDF")
	}
	if !strings.Contains(err.Error(), "xref stream") {
		t.Errorf("err = %v, want 'xref stream' mention", err)
	}
}

// makeJBIG2PDFWithGlobals writes a tiny PDF 1.4: /JBIG2Decode
// image XObject pointing at a separate /JBIG2Globals via
// /DecodeParms. Used by TestExtractWithGlobals.
func makeJBIG2PDFWithGlobals(payload, globals []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	type objRec struct {
		offset int
		body   string
	}
	objs := []objRec{
		{0, "<< /Type /Catalog /Pages 2 0 R >>"},
		{0, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"},
		{0, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] /Resources << /XObject << /Im 4 0 R >> >> >>"},
		{0, ""}, // image XObject body filled below
		{0, ""}, // globals stream body filled below
	}
	imgDict := fmt.Sprintf(
		"<< /Length %d /Type /XObject /Subtype /Image /Width 100 /Height 100 /ColorSpace /DeviceGray /Filter /JBIG2Decode /DecodeParms << /JBIG2Globals 5 0 R >> /BitsPerComponent 1 >>",
		len(payload),
	)
	objs[3].body = imgDict + "\nstream\n" + string(payload) + "\nendstream"
	globalsDict := fmt.Sprintf("<< /Length %d >>", len(globals))
	objs[4].body = globalsDict + "\nstream\n" + string(globals) + "\nendstream"
	for i := range objs {
		objs[i].offset = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, objs[i].body)
	}
	xrefOffset := buf.Len()
	buf.WriteString("xref\n")
	fmt.Fprintf(&buf, "0 %d\n", len(objs)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, o := range objs {
		fmt.Fprintf(&buf, "%010d 00000 n \n", o.offset)
	}
	buf.WriteString("trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n")
	fmt.Fprintf(&buf, "%d\n", xrefOffset)
	buf.WriteString("%%EOF\n")
	return buf.Bytes()
}

// TestExtractWithGlobals pins globals-pairing contract: extractor
// pairs JBIG2 stream with referenced /JBIG2Globals object,
// writes both as separate sidecars.
func TestExtractWithGlobals(t *testing.T) {
	pdfPath := filepath.Join(t.TempDir(), "withglobals.pdf")
	outDir := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatalf("mkdir outdir: %v", err)
	}
	payload := append(append([]byte{}, jbig2Magic...), []byte("image-stream")...)
	globals := []byte("globals-stream-bytes")
	if err := os.WriteFile(pdfPath, makeJBIG2PDFWithGlobals(payload, globals), 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	if _, err := extractOne(pdfPath, outDir, false, 0); err != nil {
		t.Fatalf("extractOne: %v", err)
	}

	gotJB2, err := os.ReadFile(filepath.Join(outDir, "withglobals-obj4.jb2"))
	if err != nil {
		t.Fatalf("read .jb2: %v", err)
	}
	if !bytes.Equal(gotJB2, payload) {
		t.Errorf("image payload mismatch:\n got %q\nwant %q", gotJB2, payload)
	}
	gotGlobals, err := os.ReadFile(filepath.Join(outDir, "withglobals-obj4.globals.jb2"))
	if err != nil {
		t.Fatalf("read globals: %v", err)
	}
	if !bytes.Equal(gotGlobals, globals) {
		t.Errorf("globals payload mismatch:\n got %q\nwant %q", gotGlobals, globals)
	}
}

// TestExtractFailOnExists pins fail-on-exists: second extraction
// into same outDir refuses to overwrite by default; --force
// re-enables.
func TestExtractFailOnExists(t *testing.T) {
	pdfPath := filepath.Join(t.TempDir(), "collide.pdf")
	outDir := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatalf("mkdir outdir: %v", err)
	}
	if err := os.WriteFile(pdfPath, makeMinimalJBIG2PDF([]byte("first")), 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	if _, err := extractOne(pdfPath, outDir, false, 0); err != nil {
		t.Fatalf("first extractOne: %v", err)
	}
	// Second pass hits existing file. force=false -> error.
	_, err := extractOne(pdfPath, outDir, false, 0)
	if err == nil {
		t.Fatal("second extractOne with force=false unexpectedly overwrote")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("err = %v, want 'refusing to overwrite' mention", err)
	}
	// force=true -> succeeds.
	if _, err := extractOne(pdfPath, outDir, true, 0); err != nil {
		t.Fatalf("third extractOne with force=true: %v", err)
	}
}

// TestExtractIndirectLength pins indirect-/Length contract: PDF
// whose image stream declares length via `/Length N 0 R`
// resolves through pdfDoc rather than getting skipped.
func TestExtractIndirectLength(t *testing.T) {
	pdfPath := filepath.Join(t.TempDir(), "indirectlen.pdf")
	outDir := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatalf("mkdir outdir: %v", err)
	}
	payload := append(append([]byte{}, jbig2Magic...), []byte("indirect-length")...)

	// PDF where /Length is /Length 5 0 R (length lives in
	// object 5 as bare integer body).
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	type objRec struct {
		offset int
		body   string
	}
	objs := []objRec{
		{0, "<< /Type /Catalog /Pages 2 0 R >>"},
		{0, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"},
		{0, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] /Resources << /XObject << /Im 4 0 R >> >> >>"},
		{0, ""},                              // image XObject body filled below
		{0, fmt.Sprintf("%d", len(payload))}, // bare-integer length object
	}
	imgDict := "<< /Length 5 0 R /Type /XObject /Subtype /Image /Width 100 /Height 100 /ColorSpace /DeviceGray /Filter /JBIG2Decode /BitsPerComponent 1 >>"
	objs[3].body = imgDict + "\nstream\n" + string(payload) + "\nendstream"
	for i := range objs {
		objs[i].offset = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, objs[i].body)
	}
	xrefOffset := buf.Len()
	buf.WriteString("xref\n")
	fmt.Fprintf(&buf, "0 %d\n", len(objs)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, o := range objs {
		fmt.Fprintf(&buf, "%010d 00000 n \n", o.offset)
	}
	buf.WriteString("trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n")
	fmt.Fprintf(&buf, "%d\n", xrefOffset)
	buf.WriteString("%%EOF\n")
	if err := os.WriteFile(pdfPath, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	n, err := extractOne(pdfPath, outDir, false, 0)
	if err != nil {
		t.Fatalf("extractOne: %v", err)
	}
	if n != 1 {
		t.Fatalf("extracted %d streams, want 1 (indirect /Length should resolve)", n)
	}
	got, err := os.ReadFile(filepath.Join(outDir, "indirectlen-obj4.jb2"))
	if err != nil {
		t.Fatalf("read .jb2: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("indirect-length extracted payload mismatch:\n got %q\nwant %q", got, payload)
	}
}

// TestExtractSkipsNonJBIG2Filters: extractor ignores image
// XObjects whose /Filter is not bare /JBIG2Decode. Filter
// chains like [/FlateDecode /JBIG2Decode] skipped per scope.
func TestExtractSkipsNonJBIG2Filters(t *testing.T) {
	pdfPath := filepath.Join(t.TempDir(), "filterchain.pdf")
	outDir := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatalf("mkdir outdir: %v", err)
	}
	// PDF with image XObject filter chain. Contrived but
	// exercises isJBIG2Image gate.
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	o1off := buf.Len()
	buf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	o2off := buf.Len()
	buf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	o3off := buf.Len()
	buf.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 10 10] /Resources << /XObject << /Im 4 0 R >> >> >>\nendobj\n")
	o4off := buf.Len()
	buf.WriteString("4 0 obj\n<< /Length 5 /Type /XObject /Subtype /Image /Filter [/FlateDecode /JBIG2Decode] /Width 10 /Height 10 /BitsPerComponent 1 >>\nstream\nabcde\nendstream\nendobj\n")
	xrefOff := buf.Len()
	buf.WriteString("xref\n0 5\n0000000000 65535 f \n")
	for _, off := range []int{o1off, o2off, o3off, o4off} {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	buf.WriteString("trailer\n<< /Size 5 /Root 1 0 R >>\nstartxref\n")
	fmt.Fprintf(&buf, "%d\n%%%%EOF\n", xrefOff)
	if err := os.WriteFile(pdfPath, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	n, err := extractOne(pdfPath, outDir, false, 0)
	if err != nil {
		t.Fatalf("extractOne: %v", err)
	}
	if n != 0 {
		t.Errorf("extracted %d streams from filter-chain PDF, want 0 (chain unsupported)", n)
	}
}

// TestExtractMaxPDFBytes pins max-pdf-bytes contract: extractOne
// stat-checks size against cap before read. Pre-read protects
// batch tooling against unexpectedly large PDFs in a corpus.
func TestExtractMaxPDFBytes(t *testing.T) {
	pdfPath := filepath.Join(t.TempDir(), "tiny.pdf")
	outDir := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatalf("mkdir outdir: %v", err)
	}
	pdf := makeMinimalJBIG2PDF([]byte("synthetic"))
	if err := os.WriteFile(pdfPath, pdf, 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}

	// Cap below PDF size -> reject before read.
	maxBytes := int64(len(pdf) - 1)
	if _, err := extractOne(pdfPath, outDir, false, maxBytes); err == nil {
		t.Error("extractOne accepted PDF larger than --max-pdf-bytes")
	} else if !strings.Contains(err.Error(), "max-pdf-bytes") {
		t.Errorf("err = %v, want 'max-pdf-bytes' mention", err)
	}

	// Cap at PDF size -> pass.
	if _, err := extractOne(pdfPath, outDir, true, int64(len(pdf))); err != nil {
		t.Errorf("at-cap extractOne failed: %v", err)
	}

	// 0 disables the check.
	if _, err := extractOne(pdfPath, outDir, true, 0); err != nil {
		t.Errorf("disabled-cap extractOne failed: %v", err)
	}
}

// TestExtractRoundTripToRealJBIG2 pins extract->decode chain:
// embed committed testdata/pdf-embedded/sample.jb2 in a minimal
// PDF, run extractOne, hand extracted .jb2 to
// gobig2.NewDecoderEmbedded, assert dims + packed-bits SHA-256
// match TestPDFEmbeddedSampleFixture.
//
// Without round-trip, extractor coverage is synthetic-byte
// equality only - regressions in stream-newline, /Length parsing,
// naming drift could land silently since no other test chains
// extractor output into a decoder.
func TestExtractRoundTripToRealJBIG2(t *testing.T) {
	const samplePath = "../../testdata/pdf-embedded/sample.jb2"
	const wantWidth = 3562
	const wantHeight = 851
	const wantSHA = "e44aad16c4c8385d0b0f7f624ba7660a5db7055ff4ae10a07e9e71e04f422695"

	sample, err := os.ReadFile(samplePath)
	if err != nil {
		t.Skipf("sample fixture unavailable: %v", err)
	}

	pdfPath := filepath.Join(t.TempDir(), "wrapped.pdf")
	outDir := filepath.Join(t.TempDir(), "out")
	if mkErr := os.MkdirAll(outDir, 0o700); mkErr != nil {
		t.Fatalf("mkdir outdir: %v", mkErr)
	}
	if wErr := os.WriteFile(pdfPath, makeMinimalJBIG2PDF(sample), 0o600); wErr != nil {
		t.Fatalf("write pdf: %v", wErr)
	}

	n, err := extractOne(pdfPath, outDir, false, 0)
	if err != nil {
		t.Fatalf("extractOne: %v", err)
	}
	if n != 1 {
		t.Fatalf("extracted %d streams, want 1", n)
	}

	extracted, err := os.ReadFile(filepath.Join(outDir, "wrapped-obj4.jb2"))
	if err != nil {
		t.Fatalf("read extracted .jb2: %v", err)
	}
	if !bytes.Equal(extracted, sample) {
		t.Fatalf("extracted bytes differ from sample input: %d vs %d bytes",
			len(extracted), len(sample))
	}

	dec, err := gobig2.NewDecoderEmbedded(bytes.NewReader(extracted), nil)
	if err != nil {
		t.Fatalf("NewDecoderEmbedded on extracted bytes: %v", err)
	}
	img, err := dec.Decode()
	if err != nil {
		t.Fatalf("Decode extracted bytes: %v", err)
	}
	if img == nil {
		t.Fatal("Decode returned nil image")
	}
	if got, want := img.Bounds().Dx(), wantWidth; got != want {
		t.Errorf("extracted-then-decoded width = %d, want %d", got, want)
	}
	if got, want := img.Bounds().Dy(), wantHeight; got != want {
		t.Errorf("extracted-then-decoded height = %d, want %d", got, want)
	}

	gray, ok := img.(*image.Gray)
	if !ok {
		t.Fatalf("expected *image.Gray, got %T", img)
	}
	if got := sha256OfGrayBits(gray); got != wantSHA {
		t.Errorf("extracted-then-decoded bitmap hash = %s, want %s", got, wantSHA)
	}
}

// sha256OfGrayBits hashes the packed 1-bit view of an image.Gray
// decoded from JBIG2. Mirrors helper in
// internal/gobig2test/corpus_pdf_test.go. Duplicated rather than
// exported because test-only.
func sha256OfGrayBits(g *image.Gray) string {
	w := g.Bounds().Dx()
	h := g.Bounds().Dy()
	stride := (w + 7) / 8
	row := make([]byte, stride)
	hasher := sha256.New()
	for y := 0; y < h; y++ {
		for i := range row {
			row[i] = 0
		}
		for x := 0; x < w; x++ {
			c := g.GrayAt(x, y).Y
			if c < 128 { // ink
				row[x>>3] |= 1 << (7 - uint(x&7))
			}
		}
		hasher.Write(row)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}
