package huffman

import (
	"testing"

	"github.com/tannevaled/gobig2/internal/bio"
)

// TestDecodeAValue32BitOffsetRejected pins: 32-bit RANGELEN line
// with offset above 0x7FFFFFFF returns -1 (malformed) vs silent
// wrap through int32(offset) to plausible-but-wrong signed value.
//
// Minimal one-code table: code `0` (1-bit prefix), RANGELEN=32,
// RANGELOW=0. Offset payload 0xFFFFFFFF. Naive int32 cast yields
// -1, RANGELOW + (-1) = -1 (legal); guarded sum
// (0 + 0xFFFFFFFF = 4294967295) rejected as > math.MaxInt32.
func TestDecodeAValue32BitOffsetRejected(t *testing.T) {
	table := &Table{
		HTOOB: false,
		CODES: []Code{
			{Codelen: 1, Code: 0, Val1: 32, Val2: 0},
			{Codelen: 1, Code: 1, Val1: 32, Val2: 0}, // UOR-escape sibling
		},
		RANGELEN: []int32{32, 32},
		RANGELOW: []int32{0, 0},
		Ok:       true,
	}
	// Stream: prefix bit `0` selects first code, then 32 bits =
	// 0xFFFFFFFF. 33 bits total = 5 bytes (bit 0 of byte 0 =
	// prefix, bytes 0-3 of rest pack offset). MSB-first.
	//
	// Bit layout:
	//   bit 0:    0       (prefix)
	//   bits 1-32: all 1s (offset = 0xFFFFFFFF)
	// -> byte 0 = 0111 1111 = 0x7F
	// -> byte 1 = 1111 1111 = 0xFF (x3)
	// -> byte 4 = 1000 0000 = 0x80 (only bit 33 used, rest pad)
	stream := bio.NewBitStream([]byte{0x7F, 0xFF, 0xFF, 0xFF, 0x80}, 0)
	dec := NewDecoder(stream)
	var result int32
	res := dec.DecodeAValue(table, &result)
	if res != -1 {
		t.Errorf("DecodeAValue with overflowing 32-bit offset returned %d (result=%d), want -1 (malformed)", res, result)
	}
}

// TestDecodeAValue32BitOffsetInRange pins negative case: 32-bit
// offset fitting int32 must decode normally, not be falsely
// rejected.
func TestDecodeAValue32BitOffsetInRange(t *testing.T) {
	table := &Table{
		HTOOB: false,
		CODES: []Code{
			{Codelen: 1, Code: 0, Val1: 32, Val2: 0},
			{Codelen: 1, Code: 1, Val1: 32, Val2: 0},
		},
		RANGELEN: []int32{32, 32},
		RANGELOW: []int32{0, 0},
		Ok:       true,
	}
	// 2-entry table HTOOB=false -> lorTail=2 so LOR-escape at
	// len(CODES)-2 = index 0 (subtractive `rlow - offset`). To
	// hit additive UOR branch (`rlow + offset`) select index 1
	// with prefix bit `1`.
	//
	// Prefix `1` + 32-bit offset = 0x0000_007F (127). MSB-first
	// packed stream bits:
	//   byte 0 (bits  0-7):  1_0000000   -> 0x80 (prefix + 7 zeros)
	//   byte 1 (bits  8-15): 00000000    -> 0x00
	//   byte 2 (bits 16-23): 00000000    -> 0x00
	//   byte 3 (bits 24-31): 00_111111   -> 0x3F (offset bits 8..1)
	//   byte 4 (bits 32-39): 1_0000000   -> 0x80 (offset bit 0 + pad)
	stream := bio.NewBitStream([]byte{0x80, 0x00, 0x00, 0x3F, 0x80}, 0)
	dec := NewDecoder(stream)
	var result int32
	res := dec.DecodeAValue(table, &result)
	if res != 0 {
		t.Fatalf("DecodeAValue with legal 32-bit offset returned %d, want 0 (ok)", res)
	}
	if result != 127 {
		t.Errorf("result = %d, want 127", result)
	}
}
