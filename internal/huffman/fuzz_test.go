package huffman

import (
	"testing"

	"github.com/tannevaled/gobig2/internal/bio"
)

// FuzzTableFromStream parses a coded Huffman-table buffer per
// T.88 §B.2. Contract: no panic, including truncated buffers
// (parser must reject vs segfault or loop on unbounded
// HTHIGH-HTLOW range).
//
// Seeds: tightest valid encodings synthesizable + bit patterns
// known to break parseFromCodedBuffer (rangelen >= 32,
// htLow > htHigh, pre-OOB-line truncation).
func FuzzTableFromStream(f *testing.F) {
	// Minimal valid: HTOOB=0, HTPS=1, HTRS=1, HTLOW=0,
	// HTHIGH=0, two LOR/UOR escape lines.
	f.Add([]byte{
		0x00,                   // flags
		0x00, 0x00, 0x00, 0x00, // HTLOW
		0x00, 0x00, 0x00, 0x00, // HTHIGH
		0x00, 0x00, 0x00, // some preflen bits
	})
	f.Add([]byte{}) // empty
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	// Random short buffer.
	f.Add(make([]byte, 16))
	f.Fuzz(func(t *testing.T, data []byte) {
		stream := bio.NewBitStream(data, 0)
		table := NewTableFromStream(stream)
		_ = table.IsOK()

		// Drive Decoder against same buffer; if table parsed OK,
		// DecodeAValue must not panic on follow-up bytes.
		if table.IsOK() {
			stream2 := bio.NewBitStream(data, 0)
			d := NewDecoder(stream2)
			var v int32
			for i := 0; i < 32; i++ {
				if !stream2.IsInBounds() {
					break
				}
				if d.DecodeAValue(table, &v) < 0 {
					break
				}
			}
		}
	})
}

// FuzzStandardTable verifies loading every standard table
// (B.1-B.15) + out-of-range indices stays panic-clean. Tiny;
// exercises NewStandardTable bounds.
func FuzzStandardTable(f *testing.F) {
	for i := 0; i < 16; i++ {
		f.Add(i)
	}
	f.Add(-1)
	f.Add(99)
	f.Fuzz(func(t *testing.T, idx int) {
		_ = NewStandardTable(idx)
	})
}
