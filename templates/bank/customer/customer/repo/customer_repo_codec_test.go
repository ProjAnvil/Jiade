package repo

import "testing"

func TestDecodeRiskTagsJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "empty array stays non-nil", raw: "[]", want: []string{}},
		{name: "tags", raw: `["pep","sanctions"]`, want: []string{"pep", "sanctions"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeRiskTagsJSON(tt.raw)
			if err != nil {
				t.Fatalf("decodeRiskTagsJSON: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("tags = %#v, want %#v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("tags = %#v, want %#v", got, tt.want)
				}
			}
		})
	}

	if _, err := decodeRiskTagsJSON(`{"not":"an array"}`); err == nil {
		t.Fatal("object risk tags must be rejected")
	}
}
