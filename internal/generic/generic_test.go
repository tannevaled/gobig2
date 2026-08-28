package generic

import (
	"bytes"
	"errors"
	"testing"

	"github.com/tannevaled/gobig2/internal/bio"
	"github.com/tannevaled/gobig2/internal/errs"
	"github.com/tannevaled/gobig2/internal/page"
	"github.com/tannevaled/gobig2/internal/state"
)

// TestDecodeMMROversize asserts per-side MaxImageSize guard
// surfaces errMMROversize (wraps errs.ErrResourceBudget) so
// document parser routes to CLI exit-code 4.
func TestDecodeMMROversize(t *testing.T) {
	g := NewProc()
	g.GBW = state.MaxImageSize + 1
	g.GBH = 64
	var img *page.Image
	_, err := g.DecodeMMR(&img, bio.NewBitStream(nil, 0))
	if err == nil {
		t.Fatal("expected error for oversized GBW")
	}
	if !errors.Is(err, errs.ErrResourceBudget) {
		t.Fatalf("oversize error should wrap ErrResourceBudget, got: %v", err)
	}
}

// TestDecodeMMRAllocFailed asserts NewImage nil (MaxImageSize-
// friendly but MaxImagePixels-hostile dims) surfaces
// errMMRAllocFailed wrapping errs.ErrResourceBudget.
func TestDecodeMMRAllocFailed(t *testing.T) {
	// Override per-bitmap pixel cap below declared region area.
	// Save + restore so shrink doesn't leak.
	prev := page.MaxImagePixels
	page.MaxImagePixels = 1024
	defer func() { page.MaxImagePixels = prev }()

	g := NewProc()
	// 2048 x 2048 = 4Mpx, above 1024 cap.
	g.GBW = 2048
	g.GBH = 2048
	var img *page.Image
	_, err := g.DecodeMMR(&img, bio.NewBitStream(nil, 0))
	if err == nil {
		t.Fatal("expected error when NewImage rejects oversized allocation")
	}
	if !errors.Is(err, errs.ErrResourceBudget) {
		t.Fatalf("allocation-failure error should wrap ErrResourceBudget, got: %v", err)
	}
}

// TestDecodeMMRMalformedStream asserts a stream passing budget
// gates but failing inside mmr.DecodeG4 surfaces raw mmr error
// (no ErrResourceBudget wrap) so document parser routes to
// ErrMalformed, not budget breach. Naive shape would wrap every
// MMR failure as ErrResourceBudget; guards that regression.
func TestDecodeMMRMalformedStream(t *testing.T) {
	// Try several byte-pattern x region-size combos. At least one
	// should hit codeword-lookup, run-length, or out-of-bits
	// branch in mmr.DecodeG4. Skip if all decode cleanly -
	// classification also pinned by DecodeMMR failure-mode doc.
	type probe struct {
		name string
		gbw  uint32
		gbh  uint32
		body []byte
	}
	probes := []probe{
		{"large-dims-tiny-payload", 4096, 4096, bytes.Repeat([]byte{0xFF}, 4)},
		{"large-dims-empty", 1024, 1024, nil},
		{"all-ones-medium", 256, 256, bytes.Repeat([]byte{0xFF}, 8)},
		{"alternating-medium", 128, 128, bytes.Repeat([]byte{0xAA, 0x55}, 4)},
	}
	for _, p := range probes {
		g := NewProc()
		g.GBW = p.gbw
		g.GBH = p.gbh
		stream := bio.NewBitStream(p.body, 0)
		var img *page.Image
		_, err := g.DecodeMMR(&img, stream)
		if err == nil {
			continue
		}
		if errors.Is(err, errs.ErrResourceBudget) {
			t.Fatalf("probe %q: malformed-stream MMR error should NOT wrap "+
				"ErrResourceBudget; that's reserved for size / allocation "+
				"failures. got: %v", p.name, err)
		}
		t.Logf("probe %q rejected with: %v", p.name, err)
		return
	}
	t.Skip("no probe hit mmr.DecodeG4's malformed-stream branches")
}
