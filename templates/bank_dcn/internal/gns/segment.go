package gns

// Segment is a DCN segment routing record.
type Segment struct {
	DCN      string `json:"dcn"`
	SegStart int    `json:"segStart"`
	SegEnd   int    `json:"segEnd"`
	Endpoint string `json:"endpoint"`
	Status   string `json:"status"`
}

// PickSegment selects the ACTIVE segment with the fewest accounts (ties broken by smallest segStart).
func PickSegment(segs []Segment, counts map[string]int) (Segment, bool) {
	var best Segment
	found := false
	for _, s := range segs {
		if s.Status != "ACTIVE" {
			continue
		}
		if !found ||
			counts[s.DCN] < counts[best.DCN] ||
			(counts[s.DCN] == counts[best.DCN] && s.SegStart < best.SegStart) {
			best, found = s, true
		}
	}
	return best, found
}

// NextAccountID computes the next account ID within the segment; returns ok=false when the segment is full.
// An empty segment allocates from SegStart+1 (SegStart itself is a routing boundary only, never an account ID).
func NextAccountID(seg Segment, maxID int, hasMax bool) (int, bool) {
	id := seg.SegStart + 1
	if hasMax {
		id = maxID + 1
	}
	if id > seg.SegEnd {
		return 0, false
	}
	return id, true
}
