package gobig2test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	gobig2 "github.com/tannevaled/gobig2"
)

// TestReservedSegmentTypeFailsMalformed pins dispatcher default
// case classifies reserved/undefined segment types as
// gobig2.ErrMalformed vs silently advancing past payload. Splices
// reserved-type-1 segment between page-info and end-of-page in
// otherwise valid standalone stream.
func TestReservedSegmentTypeFailsMalformed(t *testing.T) {
	embedded, err := os.ReadFile(pdfEmbeddedSample.path)
	if err != nil {
		t.Fatalf("read embedded fixture: %v", err)
	}
	var buf bytes.Buffer
	buf.Write([]byte{0x97, 0x4A, 0x42, 0x32, 0x0D, 0x0A, 0x1A, 0x0A, 0x03})
	buf.Write(embedded)
	// Reserved-type segment (type 1 = reserved per T.88 Annex H).
	// 11-byte sequential header with zero payload, page-associated
	// to page 1.
	buf.Write([]byte{
		0, 0, 0, 99, // segment number
		1,          // flags: type 1 (reserved), no page-assoc-size, no defer
		0,          // refByte
		1,          // page association: 1
		0, 0, 0, 0, // data length
	})
	// End-of-page + end-of-file tail.
	buf.Write([]byte{
		0, 0, 0, 100, 49, 0, 1, 0, 0, 0, 0,
		0, 0, 0, 101, 51, 0, 0, 0, 0, 0, 0,
	})
	dec, err := gobig2.NewDecoder(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("gobig2.NewDecoder: %v", err)
	}
	_, err = dec.Decode()
	if err == nil {
		t.Fatal("gobig2.Decode accepted reserved-type segment; want gobig2.ErrMalformed")
	}
	if !errors.Is(err, gobig2.ErrMalformed) {
		t.Errorf("err = %v, want errors.Is(err, gobig2.ErrMalformed)", err)
	}
}

// TestDecodeContextRetryAfterCancellation pins gobig2.Decoder whose
// prior gobig2.DecodeContext failed (pre-canceled ctx) is usable for
// retry - sticky lastErr stash cleared at start of each public
// decode attempt so retry runs clean. Without guard, 2nd call could
// re-report 1st attempt's context.Canceled even with fresh,
// uncanceled ctx.
func TestDecodeContextRetryAfterCancellation(t *testing.T) {
	data := loadStandaloneSample(t)
	dec, err := gobig2.NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gobig2.NewDecoder: %v", err)
	}

	// First attempt with already-canceled ctx: first per-segment
	// ctx check fires and routes through failf, stashing
	// context.Canceled into d.doc.lastErr.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, firstErr := dec.DecodeContext(ctx); firstErr == nil {
		t.Fatal("first gobig2.DecodeContext returned nil error; want context.Canceled")
	} else if !errors.Is(firstErr, context.Canceled) {
		t.Fatalf("first gobig2.DecodeContext err = %v, want errors.Is(err, context.Canceled)", firstErr)
	}

	// Retry with uncanceled ctx: cancel check fires before any byte
	// consumed, stream cursor still at zero. Retry must succeed -
	// bug manifestation: retry re-reports context.Canceled from
	// stale sticky stash.
	img, retryErr := dec.DecodeContext(context.Background())
	if retryErr != nil {
		t.Fatalf("retry gobig2.DecodeContext err = %v; want a clean retry "+
			"(sticky lastErr should be cleared at entry)", retryErr)
	}
	if img == nil {
		t.Fatal("retry returned nil image")
	}
}
