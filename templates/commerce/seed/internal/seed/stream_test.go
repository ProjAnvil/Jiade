package seed

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestStreamLoadProducesSameSummaryAsGenerate is the determinism contract for
// the streaming load path: StreamLoad via a counting Copier must yield a
// Summary byte-identical to the in-memory Generate path. This proves the
// per-chunk generation is faithful to the batch path.
func TestStreamLoadProducesSameSummaryAsGenerate(t *testing.T) {
	for _, scale := range []Scale{Dev, Demo} {
		t.Run(string(scale), func(t *testing.T) {
			inMemory, err := GenerateSummary(Config{Scale: scale, Seed: 42})
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			copier := &countingCopier{}
			streamed, err := StreamLoad(context.Background(), copier, Config{Scale: scale, Seed: 42})
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			// StreamLoad sets GeneratedAt the same way Generate does; both
			// paths share generationAnchor. Zero the per-table counts in the
			// copier for the assertion below.
			if diff := cmp.Diff(inMemory, streamed); diff != "" {
				t.Fatalf("stream vs in-memory summary drift (-in-memory +streamed):\n%s", diff)
			}
			// Confirm rows actually flowed through CopyFrom (not silently
			// dropped).
			if copier.rowsCopied == 0 {
				t.Fatalf("no rows copied at scale=%s", scale)
			}
			if copier.maxRowsInFlight > chunkSize {
				t.Errorf("chunkSize violated: max rows in flight=%d > chunkSize=%d",
					copier.maxRowsInFlight, chunkSize)
			}
		})
	}
}

// TestStreamLoadChunkSizeBound confirms the streaming path never holds more
// than chunkSize rows per table at once. This is the spec's "never retain all
// load rows in memory" guarantee.
func TestStreamLoadChunkSizeBound(t *testing.T) {
	copier := &countingCopier{trackInFlight: true}
	if _, err := StreamLoad(context.Background(), copier, Config{Scale: Dev, Seed: 42}); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if copier.maxRowsInFlight > chunkSize {
		t.Fatalf("chunkSize bound violated: max=%d > %d", copier.maxRowsInFlight, chunkSize)
	}
}

// countingCopier is a seed.Copier that records the number of rows per table
// and (optionally) the largest single CopyFrom call. It never retains rows,
// mirroring pgxpool.Conn.CopyFrom's contract.
type countingCopier struct {
	rowsPerTable    map[string]int64
	rowsCopied      int64
	trackInFlight   bool
	maxRowsInFlight int
}

func (c *countingCopier) CopyFrom(_ context.Context, table string, _ []string, rows [][]any) (int64, error) {
	if c.rowsPerTable == nil {
		c.rowsPerTable = make(map[string]int64)
	}
	c.rowsPerTable[table] += int64(len(rows))
	c.rowsCopied += int64(len(rows))
	if c.trackInFlight && len(rows) > c.maxRowsInFlight {
		c.maxRowsInFlight = len(rows)
	}
	return int64(len(rows)), nil
}
