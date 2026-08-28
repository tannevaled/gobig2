// Package huffman implements JBIG2 Huffman tables (T.88 Annex B,
// B.1-B.15) plus a generic decoder. Standard tables and
// user-defined parser share one Table type. Decoder.DecodeAValue
// walks the stream vs a chosen Table, returns decoded value or
// OOB sentinel.
package huffman

import (
	"sync"

	"github.com/tannevaled/gobig2/internal/bio"
)

// OOB is the out-of-bound sentinel that DecodeAValue returns when
// the matched line is the table's OOB code (HTOOB tables).
const OOB = 1

const (
	int32Max = 1<<31 - 1
	int32Min = -1 << 31
)

// Code is one Huffman code entry.
type Code struct {
	Codelen int32
	Code    int32
	Val1    int32
	Val2    int32
}

// TableLine is one row in a Huffman table definition.
type TableLine struct {
	PrefLen  int32
	RangeLen int32
	RangeLow int32
}

// Table is a Huffman table.
type Table struct {
	HTOOB    bool
	NTEMP    uint32
	CODES    []Code
	RANGELEN []int32
	RANGELOW []int32
	Ok       bool
}

// NewStandardTable returns one of the spec's standard tables
// (B.1-B.15). idx is 1-based table number.
//
// Standard tables immutable post-construction (decode reads
// CODES/RANGELEN/RANGELOW/HTOOB only), so returns shared pointer
// cached on first call. Unknown indices fall back to per-call
// construction.
//
// Real docs have multiple symbol-dict segments each selecting a
// handful of standard tables; cache keeps `parseSymbolDictInner`
// allocation-free for table-binding on subsequent dicts.
//
// **Do not mutate returned Table** - shared across callers.
// Decode is read-only; if future codepath needs mutation,
// construct fresh `&Table{}` via parseFromStandardTable.
func NewStandardTable(idx int) *Table {
	if idx >= 1 && idx <= maxStandardTable {
		standardTableOnce[idx].Do(func() {
			ht := &Table{}
			ht.parseFromStandardTable(idx)
			standardTables[idx] = ht
		})
		return standardTables[idx]
	}
	ht := &Table{}
	ht.parseFromStandardTable(idx)
	return ht
}

// maxStandardTable is the largest standard-table index defined
// by T.88 (Annex B has tables B.1 through B.15).
const maxStandardTable = 15

// standardTables holds cached pointers; index 0 unused (spec
// numbers tables 1-15). standardTableOnce gates each slot vs
// concurrent NewStandardTable races at init: one goroutine wins
// per slot, others wait.
var (
	standardTables    [maxStandardTable + 1]*Table
	standardTableOnce [maxStandardTable + 1]sync.Once
)

// NewTableFromStream builds a Table from a coded buffer per
// T.88 §B.2 (the format that table-segments carry on the wire).
func NewTableFromStream(stream *bio.BitStream) *Table {
	ht := &Table{}
	ht.parseFromCodedBuffer(stream)
	return ht
}

// Size returns the number of codes in the table.
func (h *Table) Size() uint32 {
	return uint32(len(h.CODES))
}

// IsHTOOB reports whether the table contains an out-of-bound symbol.
func (h *Table) IsHTOOB() bool {
	return h.HTOOB
}

// IsOK reports whether the Huffman table is valid.
func (h *Table) IsOK() bool {
	return h.Ok
}

// parseFromStandardTable populates the table from a standard
// definition by index.
func (h *Table) parseFromStandardTable(idx int) bool {
	if idx < 1 || idx >= len(kHuffmanTables) {
		return false
	}
	def := kHuffmanTables[idx]
	h.HTOOB = def.HTOOB
	h.NTEMP = uint32(len(def.Lines))
	h.CODES = make([]Code, h.NTEMP)
	for i := 0; i < int(h.NTEMP); i++ {
		h.CODES[i].Codelen = def.Lines[i].PrefLen
		h.CODES[i].Val1 = def.Lines[i].RangeLen
		h.CODES[i].Val2 = def.Lines[i].RangeLow
	}
	h.extendBuffers(false)
	if err := AssignCode(h.CODES); err != nil {
		h.Ok = false
	} else {
		h.Ok = true
	}
	return h.Ok
}

// parseFromCodedBuffer populates table from coded bit-stream
// buffer per T.88 §B.2.
//
// Naive shape reads flags byte as 3 sequential bit fields (1,3,4)
// MSB-first and stops before decoding any line; both mistakes
// together fail every type-53 (custom-table) segment. Correct
// flags byte uses ISO bit-position 0 = LSB:
//
//	bit 0     HTOOB
//	bits 1-3  HTPS - 1     (prefix-length field width)
//	bits 4-6  HTRS - 1     (range-length field width)
//	bit 7     reserved (0)
//
// Then HTLOW (int32) + HTHIGH (int32, highest range+1). Lines
// repeat until cumulative range covers HTHIGH; table always
// closes with two range-length-32 "lower OOB"/"upper OOB" lines,
// plus extra OOB code line when HTOOB=1.
func (h *Table) parseFromCodedBuffer(stream *bio.BitStream) bool {
	flags, headerErr := stream.Read1Byte()
	if headerErr != nil {
		return false
	}
	h.HTOOB = (flags & 0x01) != 0
	HTPS := uint32(((flags >> 1) & 0x07) + 1)
	HTRS := uint32(((flags >> 4) & 0x07) + 1)
	rawLow, headerErr := stream.ReadInteger()
	if headerErr != nil {
		return false
	}
	rawHigh, headerErr := stream.ReadInteger()
	if headerErr != nil {
		return false
	}
	htLow := int32(rawLow)
	htHigh := int32(rawHigh)
	if htLow > htHigh {
		return false
	}

	h.CODES = h.CODES[:0]
	h.RANGELEN = h.RANGELEN[:0]
	h.RANGELOW = h.RANGELOW[:0]

	// Row cap. Malicious type-53 table picks small rangelen +
	// wide [HTLOW,HTHIGH] to force many rows before downstream
	// notices. Annex B tables top at ~30 rows; 1024 leaves
	// headroom, bounds per-call allocation.
	const maxRows = 1024
	curLow := int64(htLow)
	for curLow < int64(htHigh) {
		if len(h.CODES) >= maxRows {
			return false
		}
		preflen, err := stream.ReadNBits(HTPS)
		if err != nil {
			return false
		}
		rangelen, err := stream.ReadNBits(HTRS)
		if err != nil {
			return false
		}
		// rangelen >= 32 overflows int32/int64 when forming next
		// curLow; reject defensively.
		if rangelen >= 32 {
			return false
		}
		h.CODES = append(h.CODES, Code{
			Codelen: int32(preflen),
			Val1:    int32(rangelen),
			Val2:    int32(curLow),
		})
		h.RANGELEN = append(h.RANGELEN, int32(rangelen))
		h.RANGELOW = append(h.RANGELOW, int32(curLow))
		next := curLow + (int64(1) << rangelen)
		if next > int64(int32Max) {
			return false
		}
		curLow = next
	}
	// Lower-out-of-range line: signals values strictly below HTLOW.
	preflen, err := stream.ReadNBits(HTPS)
	if err != nil {
		return false
	}
	if htLow == int32Min {
		return false
	}
	h.CODES = append(h.CODES, Code{
		Codelen: int32(preflen),
		Val1:    32,
		Val2:    htLow - 1,
	})
	h.RANGELEN = append(h.RANGELEN, 32)
	h.RANGELOW = append(h.RANGELOW, htLow-1)
	// Upper-out-of-range line: signals values at or above HTHIGH.
	preflen, err = stream.ReadNBits(HTPS)
	if err != nil {
		return false
	}
	h.CODES = append(h.CODES, Code{
		Codelen: int32(preflen),
		Val1:    32,
		Val2:    htHigh,
	})
	h.RANGELEN = append(h.RANGELEN, 32)
	h.RANGELOW = append(h.RANGELOW, htHigh)
	// Final OOB line if HTOOB=1; it has only a prefix length.
	if h.HTOOB {
		preflen, err = stream.ReadNBits(HTPS)
		if err != nil {
			return false
		}
		h.CODES = append(h.CODES, Code{Codelen: int32(preflen)})
		h.RANGELEN = append(h.RANGELEN, 0)
		h.RANGELOW = append(h.RANGELOW, 0)
	}
	h.NTEMP = uint32(len(h.CODES))
	if err := AssignCode(h.CODES); err != nil {
		return false
	}
	h.Ok = true
	return true
}

// extendBuffers grows the internal buffers.
func (h *Table) extendBuffers(increment bool) {
	h.RANGELEN = make([]int32, len(h.CODES))
	h.RANGELOW = make([]int32, len(h.CODES))
	for i := range h.CODES {
		h.RANGELEN[i] = h.CODES[i].Val1
		h.RANGELOW[i] = h.CODES[i].Val2
	}
}

// Decoder is a Huffman decoder.
type Decoder struct {
	stream *bio.BitStream
}

// NewDecoder creates a new Huffman decoder bound to a bit stream.
func NewDecoder(stream *bio.BitStream) *Decoder {
	return &Decoder{stream: stream}
}

// DecodeAValue decodes one value against the supplied table.
// Returns OOB when the matched code is the table's OOB sentinel,
// 0 on a successful in-range read (with *result populated), or -1
// on stream/format error.
func (h *Decoder) DecodeAValue(table *Table, result *int32) int {
	var val int32
	var nBits int
	for {
		if nBits > 32 {
			return -1
		}
		bit, err := h.stream.Read1Bit()
		if err != nil {
			return -1
		}
		val = (val << 1) | int32(bit)
		nBits++
		for i := 0; i < len(table.CODES); i++ {
			if table.CODES[i].Codelen != int32(nBits) || table.CODES[i].Code != val {
				continue
			}
			// Final entry of an HTOOB table is the OOB code.
			if table.HTOOB && i == len(table.CODES)-1 {
				return OOB
			}
			rlen := table.RANGELEN[i]
			rlow := table.RANGELOW[i]
			var offset uint32
			if rlen > 0 {
				v, err := h.stream.ReadNBits(uint32(rlen))
				if err != nil {
					return -1
				}
				offset = v
			}
			// LOR escape line lives at Size - (3 if HTOOB else
			// 2). Encoded offset runs DOWN from RANGELOW
			// (RANGELOW - offset), unlike normal lines and UOR
			// escape which use RANGELOW + offset.
			//
			// Compute in int64 + uint64 so 32-bit offset can't
			// wrap when cast to int32. RANGELEN==32 fields
			// (every standard escape line) must not cast uint32
			// offset to int32 directly: flips sign for offsets
			// past 0x80000000 -> plausible-but-wrong signed
			// values that bypass downstream validation. OOB
			// int32 surfaces -1 so callers tag stream malformed.
			lorTail := 2
			if table.HTOOB {
				lorTail = 3
			}
			var sum int64
			if i == len(table.CODES)-lorTail {
				sum = int64(rlow) - int64(offset)
			} else {
				sum = int64(rlow) + int64(offset)
			}
			if sum < -2147483648 || sum > 2147483647 {
				return -1
			}
			*result = int32(sum)
			return 0
		}
	}
}

// AssignCode assigns canonical Huffman codes to entries of
// symcodes in place.
//
// Kraft/prefix-validity: not checked. Length histogram may
// violate Kraft (sum_L count_L * 2^-L <= 1). Invalid tables
// produce codes overflowing at some length L; downstream lookup
// rejects unreachable codes at decode time or surfaces as
// ErrMalformed via row cap in parseFromCodedBuffer. Local Kraft
// check would duplicate signal + risk rejecting JBIG2-quirk
// tables other decoders accept; deferred until failing input.
func AssignCode(symcodes []Code) error {
	lenMax := int32(0)
	for _, sc := range symcodes {
		if sc.Codelen > lenMax {
			lenMax = sc.Codelen
		}
	}
	lenCounts := make([]int, lenMax+1)
	firstCodes := make([]int32, lenMax+1)
	for _, sc := range symcodes {
		if sc.Codelen > 0 {
			lenCounts[sc.Codelen]++
		}
	}
	lenCounts[0] = 0
	for i := int32(1); i <= lenMax; i++ {
		firstCodes[i] = (firstCodes[i-1] + int32(lenCounts[i-1])) << 1
		curCode := firstCodes[i]
		for j := range symcodes {
			if symcodes[j].Codelen == i {
				symcodes[j].Code = curCode
				curCode++
			}
		}
	}
	return nil
}

// standardTableDef is one standard Huffman-table definition.
type standardTableDef struct {
	HTOOB bool
	Lines []TableLine
}

// kHuffmanTables is the set of standard Huffman tables (T.88
// Annex B, B.1-B.15). Each entry tail follows reference decoder
// convention: tables w/o HTOOB end with LOR-escape + UOR-escape;
// tables with HTOOB end with LOR + UOR + OOB. LOR/UOR lines have
// range_length=32 and apply RANGELOW - offset (LOR) / RANGELOW +
// offset (UOR). Line with PREFLEN=0 reserves LOR slot
// unreachable - needed when source uses UOR escape only but
// decoder still needs slot at fixed index for "size - offset"
// lookup.
var kHuffmanTables = []standardTableDef{
	{false, nil},
	{false, []TableLine{{1, 4, 0}, {2, 8, 16}, {3, 16, 272}, {0, 32, -1}, {3, 32, 65808}}},
	{true, []TableLine{{1, 0, 0}, {2, 0, 1}, {3, 0, 2}, {4, 3, 3}, {5, 6, 11}, {0, 32, -1}, {6, 32, 75}, {6, 0, 0}}},
	{true, []TableLine{{8, 8, -256}, {1, 0, 0}, {2, 0, 1}, {3, 0, 2}, {4, 3, 3}, {5, 6, 11}, {8, 32, -257}, {7, 32, 75}, {6, 0, 0}}},
	{false, []TableLine{{1, 0, 1}, {2, 0, 2}, {3, 0, 3}, {4, 3, 4}, {5, 6, 12}, {0, 32, -1}, {5, 32, 76}}},
	{false, []TableLine{{7, 8, -255}, {1, 0, 1}, {2, 0, 2}, {3, 0, 3}, {4, 3, 4}, {5, 6, 12}, {7, 32, -256}, {6, 32, 76}}},
	{false, []TableLine{{5, 10, -2048}, {4, 9, -1024}, {4, 8, -512}, {4, 7, -256}, {5, 6, -128}, {5, 5, -64}, {4, 5, -32}, {2, 7, 0}, {3, 7, 128}, {3, 8, 256}, {4, 9, 512}, {4, 10, 1024}, {6, 32, -2049}, {6, 32, 2048}}},
	{false, []TableLine{{4, 9, -1024}, {3, 8, -512}, {4, 7, -256}, {5, 6, -128}, {5, 5, -64}, {4, 5, -32}, {4, 5, 0}, {5, 5, 32}, {5, 6, 64}, {4, 7, 128}, {3, 8, 256}, {3, 9, 512}, {3, 10, 1024}, {5, 32, -1025}, {5, 32, 2048}}},
	{true, []TableLine{{8, 3, -15}, {9, 1, -7}, {8, 1, -5}, {9, 0, -3}, {7, 0, -2}, {4, 0, -1}, {2, 1, 0}, {5, 0, 2}, {6, 0, 3}, {3, 4, 4}, {6, 1, 20}, {4, 4, 22}, {4, 5, 38}, {5, 6, 70}, {5, 7, 134}, {6, 7, 262}, {7, 8, 390}, {6, 10, 646}, {9, 32, -16}, {9, 32, 1670}, {2, 0, 0}}},
	{true, []TableLine{{8, 4, -31}, {9, 2, -15}, {8, 2, -11}, {9, 1, -7}, {7, 1, -5}, {4, 1, -3}, {3, 1, -1}, {3, 1, 1}, {5, 1, 3}, {6, 1, 5}, {3, 5, 7}, {6, 2, 39}, {4, 5, 43}, {4, 6, 75}, {5, 7, 139}, {5, 8, 267}, {6, 8, 523}, {7, 9, 779}, {6, 11, 1291}, {9, 32, -32}, {9, 32, 3339}, {2, 0, 0}}},
	{true, []TableLine{{7, 4, -21}, {8, 0, -5}, {7, 0, -4}, {5, 0, -3}, {2, 2, -2}, {5, 0, 2}, {6, 0, 3}, {7, 0, 4}, {8, 0, 5}, {2, 6, 6}, {5, 5, 70}, {6, 5, 102}, {6, 6, 134}, {6, 7, 198}, {6, 8, 326}, {6, 9, 582}, {6, 10, 1094}, {7, 11, 2118}, {8, 32, -22}, {8, 32, 4166}, {2, 0, 0}}},
	{false, []TableLine{{1, 0, 1}, {2, 1, 2}, {4, 0, 4}, {4, 1, 5}, {5, 1, 7}, {5, 2, 9}, {6, 2, 13}, {7, 2, 17}, {7, 3, 21}, {7, 4, 29}, {7, 5, 45}, {7, 6, 77}, {0, 32, 0}, {7, 32, 141}}},
	{false, []TableLine{{1, 0, 1}, {2, 0, 2}, {3, 1, 3}, {5, 0, 5}, {5, 1, 6}, {6, 1, 8}, {7, 0, 10}, {7, 1, 11}, {7, 2, 13}, {7, 3, 17}, {7, 4, 25}, {8, 5, 41}, {0, 32, 0}, {8, 32, 73}}},
	{false, []TableLine{{1, 0, 1}, {3, 0, 2}, {4, 0, 3}, {5, 0, 4}, {4, 1, 5}, {3, 3, 7}, {6, 1, 15}, {6, 2, 17}, {6, 3, 21}, {6, 4, 29}, {6, 5, 45}, {7, 6, 77}, {0, 32, 0}, {7, 32, 141}}},
	{false, []TableLine{{3, 0, -2}, {3, 0, -1}, {1, 0, 0}, {3, 0, 1}, {3, 0, 2}, {0, 32, -3}, {0, 32, 3}}},
	{false, []TableLine{{7, 4, -24}, {6, 2, -8}, {5, 1, -4}, {4, 0, -2}, {3, 0, -1}, {1, 0, 0}, {3, 0, 1}, {4, 0, 2}, {5, 1, 3}, {6, 2, 5}, {7, 4, 9}, {7, 32, -25}, {7, 32, 25}}},
}
