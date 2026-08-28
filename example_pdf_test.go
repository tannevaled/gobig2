package gobig2_test

import (
	"bytes"
	"fmt"
	"image"
	"os"

	gobig2 "github.com/tannevaled/gobig2"
)

// ExampleNewDecoderEmbedded_pdf shows the canonical PDF-reader
// flow: pull JBIG2Decode-filtered image stream plus optional
// /JBIG2Globals from PDF, hand both to NewDecoderEmbedded,
// Decode page bitmap.
//
// Fixture testdata/pdf-embedded/sample.jb2 extracted from real
// PDF. 94 bytes decode to 3562x851 fully-black bitmap.
func ExampleNewDecoderEmbedded_pdf() {
	// Real PDF reader: imageStream = bytes between `stream\n`
	// and `\nendstream` of Image XObject with /Filter
	// /JBIG2Decode. globalsBytes = /JBIG2Globals from
	// /DecodeParms; nil when image dict has no reference.
	imageStream, err := os.ReadFile("testdata/pdf-embedded/sample.jb2")
	if err != nil {
		fmt.Println(err)
		return
	}
	var globalsBytes []byte // pulled from /JBIG2Globals if present

	dec, err := gobig2.NewDecoderEmbedded(bytes.NewReader(imageStream), globalsBytes)
	if err != nil {
		// Adversarial / non-JBIG2 rejected up front, before any
		// allocation from declared dimensions. Surface to PDF
		// reader's per-image error path; do not panic.
		fmt.Println("decode error:", err)
		return
	}

	img, err := dec.Decode()
	if err != nil {
		fmt.Println("decode error:", err)
		return
	}

	// img is *image.Gray; ink = 0 (black), paper = 255 (white).
	// PDF /Width and /Height should match JBIG2 page-info
	// dimensions; if not, trust JBIG2 stream and scale via CTM.
	g := img.(*image.Gray)
	fmt.Printf("decoded %dx%d\n", g.Bounds().Dx(), g.Bounds().Dy())
	// Output: decoded 3562x851
}
