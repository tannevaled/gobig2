package arith

import (
	"strings"
	"testing"

	"github.com/tannevaled/gobig2/internal/bio"
)

// TestClampedIaidCodeLen pins SBSYMCODELEN clamping math without
// NewIaidDecoder, which would allocate 1<<MaxIaidCodeLen (~2 GiB
// at cap) and make the test memory-fragile.
func TestClampedIaidCodeLen(t *testing.T) {
	cases := []struct {
		in   uint8
		want uint8
	}{
		{0, 0},
		{1, 1},
		{MaxIaidCodeLen - 1, MaxIaidCodeLen - 1},
		{MaxIaidCodeLen, MaxIaidCodeLen},
		{MaxIaidCodeLen + 1, MaxIaidCodeLen},
		{255, MaxIaidCodeLen},
	}
	for _, c := range cases {
		if got := clampedIaidCodeLen(c.in); got != c.want {
			t.Errorf("clampedIaidCodeLen(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestNewIaidDecoderPreservesSBSYMCODELEN pins: NewIaidDecoder
// retains original sbsymCodeLen field even when allocation
// clamped. Decode loop then walks past clamped slice and trips
// bounds check vs panic.
//
// Small widths only so allocation stays bounded; clamping math
// covered by TestClampedIaidCodeLen without allocation.
func TestNewIaidDecoderPreservesSBSYMCODELEN(t *testing.T) {
	d := NewIaidDecoder(4)
	if d == nil {
		t.Fatal("NewIaidDecoder returned nil")
	}
	if d.sbsymCodeLen != 4 {
		t.Errorf("sbsymCodeLen = %d, want 4", d.sbsymCodeLen)
	}
	if len(d.iaid) != 1<<4 {
		t.Errorf("len(iaid) = %d, want %d", len(d.iaid), 1<<4)
	}
}

// TestIaidDecodeOversizeReturnsError pins: decode-loop bounds
// check fires with "index out of bounds" when retained
// sbsymCodeLen exceeds iaid array length. Constructs IaidDecoder
// directly with small slice + large width, avoids
// 1<<MaxIaidCodeLen allocation.
func TestIaidDecodeOversizeReturnsError(t *testing.T) {
	// Small slice + large width: loop walks via
	// `prev = (prev << 1) | bit` from prev=1, indexes iaid[prev]
	// each iter. len(iaid)=4, sbsymCodeLen=8 -> hits bounds well
	// before completing.
	iaid := &IaidDecoder{
		iaid:         make([]Ctx, 4),
		sbsymCodeLen: 8,
	}
	stream := bio.NewBitStream([]byte{0x00, 0x00, 0x00, 0x00}, 0)
	dec := NewDecoder(stream)
	_, err := iaid.Decode(dec)
	if err == nil {
		t.Fatal("oversize IAID Decode did not error")
	}
	if !strings.Contains(err.Error(), "index out of bounds") {
		t.Errorf("err = %q, want 'index out of bounds'", err)
	}
}
