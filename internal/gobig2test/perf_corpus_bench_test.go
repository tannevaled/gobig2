package gobig2test

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	gobig2 "github.com/tannevaled/gobig2"
)

// perfCorpusDir holds the synthesized fixture matrix built by
// [scripts/perf/build-perf-testdata.sh](../../scripts/perf/build-perf-testdata.sh):
// 600 / 300 dpi A4 pages encoded under jbig2enc's main flag
// combinations (generic / generic+TPGD / symbol) plus
// classifier-tuning specials. The shape is large enough that
// per-call parser overhead is dwarfed by decode work, so each
// row in the bench output is a usable signal on its own -
// unlike the bundled SerenityOS fixtures whose 1-10 ms decode
// budgets are setup-cost dominated.
const perfCorpusDir = "../../testdata/perf"

// BenchmarkPerfCorpus walks testdata/perf/*.jb2 in name order
// and decodes each in a sub-bench. SetBytes is sized in
// decoded pixel-bytes (width*height/8) so the "MB/s" column
// compares across encoder choices at a single pixel-throughput
// scale. A symbol-mode fixture is ~5x smaller on disk than its
// generic-region twin for the same source page; pixel-throughput
// normalizes the two so the column reads "how fast did we
// reconstruct the page" rather than "how fast did we consume
// the input bytes."
//
// Per-iteration shape mirrors `BenchmarkPerImageGlobalsReparse`
// (new gobig2.Decoder per Decode call): represents the cold-
// path PDF-reader pattern. For hot-loop comparisons across
// commits, prefer `-benchtime=Nx` over `-benchtime=Ns` so the
// per-fixture iteration count stays stable.
func BenchmarkPerfCorpus(b *testing.B) {
	matches, err := filepath.Glob(filepath.Join(perfCorpusDir, "*.jb2"))
	if err != nil {
		b.Fatalf("glob %s: %v", perfCorpusDir, err)
	}
	if len(matches) == 0 {
		b.Skipf("no fixtures in %s; run scripts/perf/build-perf-testdata.sh", perfCorpusDir)
	}
	sort.Strings(matches)
	for _, path := range matches {
		name := strings.TrimSuffix(filepath.Base(path), ".jb2")
		data, err := os.ReadFile(path)
		if err != nil {
			b.Fatalf("%s: read: %v", name, err)
		}
		// Pre-decode once outside the timed loop to size
		// SetBytes by pixel-bytes. Decoder is cheap to build
		// twice; the alternative (parsing the segment table by
		// hand to read width/height) duplicates parser logic.
		dec, err := gobig2.NewDecoder(bytes.NewReader(data))
		if err != nil {
			b.Fatalf("%s: ctor (sizing): %v", name, err)
		}
		img, err := dec.Decode()
		if err != nil {
			b.Fatalf("%s: decode (sizing): %v", name, err)
		}
		bounds := img.Bounds()
		pixelBytes := int64(bounds.Dx()*bounds.Dy()) / 8
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(pixelBytes)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dec, err := gobig2.NewDecoder(bytes.NewReader(data))
				if err != nil {
					b.Fatalf("ctor: %v", err)
				}
				if _, err := dec.Decode(); err != nil {
					b.Fatalf("decode: %v", err)
				}
			}
		})
	}
}
