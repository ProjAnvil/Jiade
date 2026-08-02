package gns

import "testing"

func segs() []Segment {
	return []Segment{
		{DCN: "dcn01", SegStart: 1000, SegEnd: 1999, Status: "ACTIVE"},
		{DCN: "dcn02", SegStart: 2000, SegEnd: 2999, Status: "ACTIVE"},
		{DCN: "dcn03", SegStart: 3000, SegEnd: 3999, Status: "DRAINING"},
	}
}

func TestPickSegmentMinCount(t *testing.T) {
	got, ok := PickSegment(segs(), map[string]int{"dcn01": 5, "dcn02": 2})
	if !ok || got.DCN != "dcn02" {
		t.Fatalf("PickSegment = %v,%v, want dcn02", got.DCN, ok)
	}
}

func TestPickSegmentSkipsDrainingAndTieBreaksBySegStart(t *testing.T) {
	got, ok := PickSegment(segs(), map[string]int{"dcn01": 2, "dcn02": 2, "dcn03": 0})
	if !ok || got.DCN != "dcn01" {
		t.Fatalf("PickSegment = %v,%v, want dcn01 (DRAINING 不参与，并列取号段小者)", got.DCN, ok)
	}
}

func TestNextAccountID(t *testing.T) {
	seg := Segment{DCN: "dcn01", SegStart: 1000, SegEnd: 1999}
	if id, ok := NextAccountID(seg, 0, false); !ok || id != 1001 {
		t.Fatalf("empty segment should start at 1001 (SegStart 为路由边界), got %d,%v", id, ok)
	}
	if id, ok := NextAccountID(seg, 1007, true); !ok || id != 1008 {
		t.Fatalf("next after 1007 should be 1008, got %d,%v", id, ok)
	}
	if _, ok := NextAccountID(seg, 1999, true); ok {
		t.Fatal("segment full should return ok=false")
	}
}
