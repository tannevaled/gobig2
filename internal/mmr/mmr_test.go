package mmr

import (
	"errors"
	"testing"

	"github.com/tannevaled/gobig2/internal/bio"
	"github.com/tannevaled/gobig2/internal/errs"
	"github.com/tannevaled/gobig2/internal/page"
)

// TestUncompressZeroDimensions pins that halftone-region MMR
// (drives Uncompress with HGW/HGH from stream) surfaces
// errAllocFailed wrapping errs.ErrResourceBudget on adversarial
// 0 / negative dims vs panic on img.Fill after NewImage nil.
func TestUncompressZeroDimensions(t *testing.T) {
	cases := []struct {
		name   string
		width  int
		height int
	}{
		{"zero width", 0, 100},
		{"zero height", 100, 0},
		{"both zero", 0, 0},
		{"negative width", -1, 100},
		{"negative height", 100, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dec := NewDecompressor(c.width, c.height, bio.NewBitStream(nil, 0))
			img, err := dec.Uncompress()
			if err == nil {
				t.Fatalf("Uncompress accepted invalid dims (w=%d h=%d); want error", c.width, c.height)
			}
			if img != nil {
				t.Errorf("expected nil image alongside error, got %v", img)
			}
			if !errors.Is(err, errs.ErrResourceBudget) {
				t.Errorf("invalid-dim error should wrap ErrResourceBudget, got: %v", err)
			}
		})
	}
}

// TestUncompressMaxImagePixelsRejection pins that
// page.MaxImagePixels rejections inside Uncompress surface as
// ErrResourceBudget vs panic. Pre-fix code called img.Fill on
// the nil image returned when cap fired.
func TestUncompressMaxImagePixelsRejection(t *testing.T) {
	prev := page.MaxImagePixels
	page.MaxImagePixels = 1024
	defer func() { page.MaxImagePixels = prev }()

	dec := NewDecompressor(2048, 2048, bio.NewBitStream(nil, 0))
	img, err := dec.Uncompress()
	if err == nil {
		t.Fatal("Uncompress accepted dims past MaxImagePixels; want error")
	}
	if img != nil {
		t.Errorf("expected nil image alongside error, got %v", img)
	}
	if !errors.Is(err, errs.ErrResourceBudget) {
		t.Errorf("MaxImagePixels rejection should wrap ErrResourceBudget, got: %v", err)
	}
}
