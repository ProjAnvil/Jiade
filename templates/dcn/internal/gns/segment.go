package gns

// Segment 是一个 DCN 号段路由记录。
type Segment struct {
	DCN      string `json:"dcn"`
	SegStart int    `json:"segStart"`
	SegEnd   int    `json:"segEnd"`
	Endpoint string `json:"endpoint"`
	Status   string `json:"status"`
}

// PickSegment 在 ACTIVE 号段中选账户数最少者（并列取 segStart 最小者）。
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

// NextAccountID 计算段内下一个账号；号段满返回 ok=false。
// 空号段从 SegStart+1 起分配（SegStart 本身仅作路由边界，不作账号）。
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
