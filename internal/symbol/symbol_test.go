package symbol

import (
	"testing"

	"github.com/tannevaled/gobig2/internal/arith"
	"github.com/tannevaled/gobig2/internal/page"
)

// TestDictBasics covers the Dict data structure end-to-end.
func TestDictBasics(t *testing.T) {
	d := NewDict()
	if d == nil {
		t.Fatal("NewDict returned nil")
	}
	if d.NumImages() != 0 {
		t.Errorf("NumImages on fresh Dict = %d, want 0", d.NumImages())
	}
	if got := d.GetImage(0); got != nil {
		t.Errorf("GetImage(0) on empty Dict = %v, want nil", got)
	}
	if got := d.GetImage(-1); got != nil {
		t.Errorf("GetImage(-1) = %v, want nil", got)
	}

	img1 := page.NewImage(10, 10)
	img2 := page.NewImage(20, 20)
	d.AddImage(img1)
	d.AddImage(img2)
	d.AddImage(nil) // legitimate: zero-pixel symbols become nil entries

	if d.NumImages() != 3 {
		t.Errorf("NumImages after 3 adds = %d, want 3", d.NumImages())
	}
	if d.GetImage(0) != img1 {
		t.Errorf("GetImage(0) = %v, want %v", d.GetImage(0), img1)
	}
	if d.GetImage(2) != nil {
		t.Errorf("GetImage(2) = %v, want nil", d.GetImage(2))
	}
	if d.GetImage(3) != nil {
		t.Errorf("GetImage(3) (out of range) = %v, want nil", d.GetImage(3))
	}

	// Context accessors.
	gb := make([]arith.Ctx, 4)
	gr := make([]arith.Ctx, 8)
	d.SetGbContexts(gb)
	d.SetGrContexts(gr)
	if got := d.GbContexts(); len(got) != 4 {
		t.Errorf("GbContexts len = %d, want 4", len(got))
	}
	if got := d.GrContexts(); len(got) != 8 {
		t.Errorf("GrContexts len = %d, want 8", len(got))
	}
}

// TestDictDeepCopy verifies DeepCopy is independent; mutations
// don't bleed across.
func TestDictDeepCopy(t *testing.T) {
	src := NewDict()
	src.AddImage(page.NewImage(8, 8))
	src.AddImage(nil)
	src.SetGbContexts(make([]arith.Ctx, 2))
	src.SetGrContexts(make([]arith.Ctx, 3))

	dst := src.DeepCopy()
	if dst.NumImages() != src.NumImages() {
		t.Fatalf("DeepCopy NumImages = %d, want %d", dst.NumImages(), src.NumImages())
	}
	if dst.GetImage(0) == src.GetImage(0) {
		t.Error("DeepCopy preserved pointer identity for images; want fresh copies")
	}
	if dst.GetImage(1) != nil {
		t.Errorf("DeepCopy of nil image = %v, want nil", dst.GetImage(1))
	}
	if len(dst.GbContexts()) != len(src.GbContexts()) {
		t.Errorf("DeepCopy GbContexts len = %d, want %d",
			len(dst.GbContexts()), len(src.GbContexts()))
	}
	if len(dst.GrContexts()) != len(src.GrContexts()) {
		t.Errorf("DeepCopy GrContexts len = %d, want %d",
			len(dst.GrContexts()), len(src.GrContexts()))
	}

	// Mutate src; dst should not change.
	src.AddImage(page.NewImage(4, 4))
	if dst.NumImages() == src.NumImages() {
		t.Error("DeepCopy shared backing array with source")
	}
}

// TestCheckTRDDimension covers (dimension + delta) bounds check
// vs uint32/int64 overflow.
func TestCheckTRDDimension(t *testing.T) {
	cases := []struct {
		name      string
		dim       uint32
		delta     int32
		want      uint32
		wantValid bool
	}{
		{"zero plus zero", 0, 0, 0, true},
		{"small positive delta", 100, 50, 150, true},
		{"small negative delta", 100, -50, 50, true},
		{"max plus zero", 0xFFFFFFFF, 0, 0xFFFFFFFF, true},
		{"underflow rejected", 100, -200, 0, false},
		{"overflow rejected", 0xFFFFFFFF, 1, 0, false},
		{"large negative drives below zero", 0, -1, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := checkTRDDimension(tc.dim, tc.delta)
			if ok != tc.wantValid {
				t.Errorf("checkTRDDimension(%d, %d): valid=%v, want %v",
					tc.dim, tc.delta, ok, tc.wantValid)
			}
			if tc.wantValid && got != tc.want {
				t.Errorf("checkTRDDimension(%d, %d) = %d, want %d",
					tc.dim, tc.delta, got, tc.want)
			}
		})
	}
}

// TestCheckTRDReferenceDimension covers (offset + dimension >> shift)
// helper for per-instance refinement branch.
func TestCheckTRDReferenceDimension(t *testing.T) {
	cases := []struct {
		name      string
		dim       int32
		shift     uint32
		offset    int32
		want      int32
		wantValid bool
	}{
		{"zero", 0, 0, 0, 0, true},
		{"shift 1", 100, 1, 0, 50, true},
		{"shift 1 with offset", 100, 1, 10, 60, true},
		{"max int32 dimension", 1000, 0, 0, 1000, true},
		{"negative offset", 100, 0, -50, 50, true},
		// Extreme values to exercise the int64 bounds check.
		{"int32 max + max overflow", 2147483647, 0, 1, 0, false},
		{"large negative offset underflow", 0, 0, -2147483648, -2147483648, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := checkTRDReferenceDimension(tc.dim, tc.shift, tc.offset)
			if ok != tc.wantValid {
				t.Errorf("checkTRDReferenceDimension(%d, %d, %d): valid=%v, want %v",
					tc.dim, tc.shift, tc.offset, ok, tc.wantValid)
			}
			if tc.wantValid && got != tc.want {
				t.Errorf("checkTRDReferenceDimension(%d, %d, %d) = %d, want %d",
					tc.dim, tc.shift, tc.offset, got, tc.want)
			}
		})
	}
}

// TestGetComposeData covers each (TRANSPOSED x REFCORNER)
// combo of placement helper. Math simple but 8-way branch
// silently breaks on refactor.
func TestGetComposeData(t *testing.T) {
	type want struct {
		x, y, increment int32
	}
	cases := []struct {
		name       string
		transposed bool
		corner     Corner
		si, ti     int32
		wi, hi     uint32
		want       want
	}{
		// Non-transposed.
		{
			"upright TopLeft", false, CornerTopLeft, 100, 200, 30, 40,
			want{100, 200, 29},
		},
		{
			"upright TopRight", false, CornerTopRight, 100, 200, 30, 40,
			want{71, 200, 29},
		},
		{
			"upright BottomLeft", false, CornerBottomLeft, 100, 200, 30, 40,
			want{100, 161, 29},
		},
		{
			"upright BottomRight", false, CornerBottomRight, 100, 200, 30, 40,
			want{71, 161, 29},
		},
		// Transposed: x/y axes swap, WI/HI roles swap.
		{
			"transposed TopLeft", true, CornerTopLeft, 100, 200, 30, 40,
			want{200, 100, 39},
		},
		{
			"transposed TopRight", true, CornerTopRight, 100, 200, 30, 40,
			want{200, 71, 39},
		},
		{
			"transposed BottomLeft", true, CornerBottomLeft, 100, 200, 30, 40,
			want{161, 100, 39},
		},
		{
			"transposed BottomRight", true, CornerBottomRight, 100, 200, 30, 40,
			want{161, 71, 39},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTRDProc()
			tr.TRANSPOSED = tc.transposed
			tr.REFCORNER = tc.corner
			got := tr.GetComposeData(tc.si, tc.ti, tc.wi, tc.hi)
			if got.x != tc.want.x || got.y != tc.want.y || got.increment != tc.want.increment {
				t.Errorf("GetComposeData = {x=%d,y=%d,inc=%d}, want {x=%d,y=%d,inc=%d}",
					got.x, got.y, got.increment,
					tc.want.x, tc.want.y, tc.want.increment)
			}
		})
	}
}
