package probe

import (
	"bytes"
	"fmt"

	"github.com/tannevaled/gobig2/internal/errs"
	"github.com/tannevaled/gobig2/internal/segment"
)

// Magic is the standalone JBIG2 file-header signature from T.88 Annex E.
var Magic = []byte{0x97, 0x4A, 0x42, 0x32, 0x0D, 0x0A, 0x1A, 0x0A}

// Config is the byte-level organization metadata recovered
// from a standalone JBIG2 header by [Configs]. Data holds
// input bytes positioned at the first segment header (past
// the file header and optional page-count word).
type Config struct {
	Data         []byte
	RandomAccess bool
	LittleEndian bool
	OrgMode      int
	Grouped      bool
}

// RejectUnsupportedOrg short-circuits public constructors on
// JBIG2 organization modes parser can't serve. T.88
// random-access mode (org-type bit set, sequential mode 0)
// places all headers before payload with per-segment numbers,
// page assocs, and separate data-offset table.
// ParseSegmentHeader reads segment numbers only under
// OrgMode==1 (grouped) or non-random-access sequential, so
// sequential random-access segments all end up numbered zero
// and SDINSYMS / refinement refs collapse. No corpus fixture
// exercises this; reject with [errs.ErrUnsupported] rather
// than emit silently wrong output.
func RejectUnsupportedOrg(randomAccess bool, orgMode int) error {
	if randomAccess && orgMode != 1 {
		return fmt.Errorf("jbig2: random-access sequential organization (T.88 Annex E mode 0) not implemented: %w", errs.ErrUnsupported)
	}
	return nil
}

// DetectEmbeddedEndianness applies the embedded-mode byte-order
// heuristic: 4-byte prefix shaped `nz 0 0 0` (non-zero + three
// zeros) flags little-endian. Shorter than 4 defaults big-endian.
func DetectEmbeddedEndianness(data []byte) bool {
	return len(data) >= 4 && data[0] != 0 && data[1] == 0 && data[2] == 0 && data[3] == 0
}

// SniffEmbeddedJBIG2 inspects the first segment header to
// reject input not shaped like a JBIG2 segment stream. Not a
// full validator - bounces plain text / random JPEG bytes
// before document loop.
//
// First-segment checks:
//
//   - 11 bytes min (segNum 4 + flags 1 + refByte 1 +
//     page-assoc 1 + dataLen 4).
//   - Segment type (flags & 0x3F) must be defined per
//     T.88 / ISO/IEC 14492 Annex H. Reserved/undefined fail.
//   - dataLen shall not wildly exceed total input. Streaming
//     sentinel 0xFFFFFFFF allowed; any other > `len(data)*4`
//     treated as garbage.
//
// Endianness detected via [DetectEmbeddedEndianness] so dataLen
// check reads with correct byte order; without that, LE inputs
// would be judged against BE reading.
func SniffEmbeddedJBIG2(data []byte) error {
	if len(data) < 11 {
		return fmt.Errorf("jbig2: input too short to contain a segment header: %w", errs.ErrMalformed)
	}
	segType := data[4] & 0x3F
	if !ValidSegmentType(segType) {
		return fmt.Errorf("jbig2: input does not look like a segment stream (invalid first segment type): %w", errs.ErrMalformed)
	}
	// Parse enough header to find dataLen. refCount in top 3
	// bits of refByte; long form (>=5 refs) uses extended
	// encoding we skip - sniff defers to parser there.
	refByte := data[5]
	refCount := int(refByte >> 5)
	if refCount == 7 {
		return nil // long-form refs; defer to parser
	}
	flags := data[4]
	pageAssocSize := 1
	if flags&0x40 != 0 {
		pageAssocSize = 4
	}
	// Sequential mode: 1-byte refs (segNum size depends on
	// random-access tables we don't have; sniff only needs
	// dataLen FIELD position). Estimate gates garbage; ref-size
	// mismatches surface in parser, no worse than unsniffed.
	headerSize := 4 + 1 + 1 + refCount*1 + pageAssocSize + 4
	if len(data) < headerSize {
		return fmt.Errorf("jbig2: input too short for first segment's declared header layout: %w", errs.ErrMalformed)
	}
	var dataLen uint32
	if DetectEmbeddedEndianness(data) {
		dataLen = uint32(data[headerSize-1])<<24 |
			uint32(data[headerSize-2])<<16 |
			uint32(data[headerSize-3])<<8 |
			uint32(data[headerSize-4])
	} else {
		dataLen = uint32(data[headerSize-4])<<24 |
			uint32(data[headerSize-3])<<16 |
			uint32(data[headerSize-2])<<8 |
			uint32(data[headerSize-1])
	}
	if dataLen != 0xFFFFFFFF && uint64(dataLen) > uint64(len(data))*4 {
		return fmt.Errorf("jbig2: first segment declares implausible data length: %w", errs.ErrMalformed)
	}
	return nil
}

// ValidSegmentType reports whether t is defined by T.88 /
// ISO/IEC 14492 Annex H. Reserved/undefined fail. Backed by
// [segment.TypeInfo] so dispatcher, sniff, CLI stay in lockstep.
func ValidSegmentType(t byte) bool {
	return segment.IsDefinedType(t)
}

// Configs probes a standalone JBIG2 file header and recovers
// the org metadata the parser needs (random-access vs
// sequential, byte order, OrgMode, grouped layout). Nil if
// input lacks standalone [Magic].
//
// Walks fixed candidate table covering every legal combo of
// (offset, randomAccess, littleEndian, OrgMode), scores each
// vs first segment header plausibility, picks highest. Score:
//
//   - +50 if dataLen <= remaining bytes, -80 otherwise.
//   - +10 if RandomAccess matches data[8] org-type bit.
//   - +40 if bytes past first header look like a second header
//     (small non-zero segNum + defined segType). Detects
//     grouped layout (all headers before all data); sequential
//     wouldn't have a header at that offset.
//
// Ties resolve in table order. Table puts RA+OrgMode=0 first
// so [RejectUnsupportedOrg] can bounce it explicitly rather
// than letting tied OrgMode=1 win and mis-parse.
func Configs(data []byte) *Config {
	if len(data) < len(Magic) || !bytes.HasPrefix(data, Magic) {
		return nil
	}
	type candidateConfig struct {
		Offset       int
		RandomAccess bool
		LittleEndian bool
		OrgMode      int
		Grouped      bool
	}
	var validConfig *candidateConfig
	bestScore := -1
	candidates := []candidateConfig{
		{9, true, false, 0, false},
		{9, false, false, 0, false},
		{9, true, false, 1, false},
		{13, false, false, 0, false},
		{13, false, true, 0, false},
		{9, false, true, 0, false},
	}
	for _, cfg := range candidates {
		if len(data) <= cfg.Offset+5 {
			continue
		}
		hasPageCount := (data[8] & 0x02) == 0
		if hasPageCount && cfg.Offset == 9 {
			continue
		}
		if !hasPageCount && cfg.Offset == 13 {
			continue
		}
		hStart := 0
		var segNum uint32
		if cfg.OrgMode == 1 || !cfg.RandomAccess {
			hStart = 4
			if len(data) <= cfg.Offset+4 {
				continue
			}
			s1, s2, s3, s4 := uint32(data[cfg.Offset]), uint32(data[cfg.Offset+1]), uint32(data[cfg.Offset+2]), uint32(data[cfg.Offset+3])
			if cfg.LittleEndian {
				segNum = s1 | (s2 << 8) | (s3 << 16) | (s4 << 24)
			} else {
				segNum = (s1 << 24) | (s2 << 16) | (s3 << 8) | s4
			}
		}
		if len(data) <= cfg.Offset+hStart {
			continue
		}
		flagsByte := data[cfg.Offset+hStart]
		hStart++
		_ = flagsByte & 0x3F
		pageAssocSize := (flagsByte & 0x40) != 0
		if len(data) <= cfg.Offset+hStart {
			continue
		}
		refByte := data[cfg.Offset+hStart]
		hStart++
		refCount := int(refByte >> 5)
		if refCount == 7 {
			continue
		}
		segNumSizeBytes := 1
		if !cfg.RandomAccess || cfg.OrgMode == 1 {
			if segNum > 65536 {
				segNumSizeBytes = 4
			} else if segNum > 256 {
				segNumSizeBytes = 2
			}
		}
		if refCount > 0 {
			hStart += refCount * segNumSizeBytes
		}
		if cfg.OrgMode == 1 || !cfg.RandomAccess {
			if pageAssocSize {
				hStart += 4
			} else {
				hStart++
			}
		}
		if len(data) <= cfg.Offset+hStart+3 {
			continue
		}
		dl1, dl2, dl3, dl4 := uint32(data[cfg.Offset+hStart]), uint32(data[cfg.Offset+hStart+1]), uint32(data[cfg.Offset+hStart+2]), uint32(data[cfg.Offset+hStart+3])
		var dataLen uint32
		if cfg.LittleEndian {
			dataLen = dl1 | (dl2 << 8) | (dl3 << 16) | (dl4 << 24)
		} else {
			dataLen = (dl1 << 24) | (dl2 << 16) | (dl3 << 8) | dl4
		}
		remaining := len(data) - (cfg.Offset + hStart + 4)
		score := 0
		if int(dataLen) <= remaining {
			score += 50
		} else {
			score -= 80
		}
		if dataLen > 0 {
			score += 10
		}
		declaredRandom := (data[8] & 0x01) != 0
		if cfg.RandomAccess == declaredRandom {
			score += 10
		}
		candidateGrouped := false
		hSize := hStart + 4
		gIdx := cfg.Offset + hSize
		if gIdx+5 < len(data) {
			n1, n2, n3, n4 := data[gIdx], data[gIdx+1], data[gIdx+2], data[gIdx+3]
			nSeg := uint32(n1)<<24 | uint32(n2)<<16 | uint32(n3)<<8 | uint32(n4)
			nType := data[gIdx+4] & 0x3F
			if nSeg > 0 && nSeg < 1000 && nType <= 62 && nType != 0 {
				candidateGrouped = true
				score += 40
			}
		}
		if score > bestScore {
			bestScore = score
			validConfig = &candidateConfig{cfg.Offset, cfg.RandomAccess, cfg.LittleEndian, cfg.OrgMode, candidateGrouped}
		}
	}
	if validConfig == nil {
		return nil
	}
	return &Config{
		Data:         data[validConfig.Offset:],
		RandomAccess: validConfig.RandomAccess,
		LittleEndian: validConfig.LittleEndian,
		OrgMode:      validConfig.OrgMode,
		Grouped:      validConfig.Grouped,
	}
}
