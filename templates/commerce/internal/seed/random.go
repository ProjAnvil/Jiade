package seed

import (
	"fmt"
	"hash/fnv"
	"math/rand"
)

// domainStream returns a deterministic *rand.Rand seeded solely from
// (Config.Seed, domain). Domains are isolated so that adding a field to the
// customer generator never reshuffles order outcomes.
//
// We use the FNV-1a hash to mix the seed with the domain tag, then seed a
// rand.NewSource. rand.Rand is not safe for concurrent use; Generate runs
// single-threaded, so this is fine.
func domainStream(seed int64, domain string) *rand.Rand {
	mixer := fnv.New64a()
	_, _ = mixer.Write([]byte(domain))
	_, _ = mixer.Write([]byte(fmt.Sprintf(":%d", seed)))
	return rand.New(rand.NewSource(int64(mixer.Sum64())))
}

// pick returns vocabulary[index % len(vocabulary)] so we never panic on empty
// slices and selection is fully determined by the stream.
func pick[T any](stream *rand.Rand, vocabulary []T) T {
	var zero T
	if len(vocabulary) == 0 {
		return zero
	}
	return vocabulary[stream.Intn(len(vocabulary))]
}

// pickN deterministically draws n distinct indices from [0, size) using a
// partial Fisher-Yates on a freshly seeded stream-local permutation. It panics
// if n > size so callers surface bad counts immediately.
func pickN(stream *rand.Rand, size, n int) []int {
	if n < 0 || n > size {
		panic(fmt.Sprintf("pickN: n=%d size=%d", n, size))
	}
	perm := stream.Perm(size)
	return perm[:n]
}

// intRange returns a value in [min, max] inclusive.
func intRange(stream *rand.Rand, min, max int) int {
	if max < min {
		min, max = max, min
	}
	return min + stream.Intn(max-min+1)
}

// int64Range returns a value in [min, max] inclusive.
func int64Range(stream *rand.Rand, min, max int64) int64 {
	if max < min {
		min, max = max, min
	}
	return min + stream.Int63n(max-min+1)
}

// minorRange is a convenience for monetary values that must stay positive.
func minorRange(stream *rand.Rand, minMinor, maxMinor int64) int64 {
	if minMinor < 0 {
		minMinor = 0
	}
	return int64Range(stream, minMinor, maxMinor)
}

// zipfWeight returns a Zipf-like biased index in [0, size) so popular items
// dominate. s=1.5 skews demand toward the head without over-fitting.
func zipfIndex(stream *rand.Rand, size int) int {
	if size <= 0 {
		return 0
	}
	z := rand.NewZipf(stream, 1.5, 1.0, uint64(size-1))
	return int(z.Uint64())
}

// boolP returns true with probability p in [0, 1].
func boolP(stream *rand.Rand, p float64) bool {
	return stream.Float64() < p
}
