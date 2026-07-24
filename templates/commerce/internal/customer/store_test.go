package customer

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCanonicalCustomerUsesUTCForPublicTimestamp(t *testing.T) {
	chinaStandardTime := time.FixedZone("CST", 8*60*60)
	customer := Customer{
		ID:        "CUS-1",
		CreatedAt: time.Date(2026, 7, 24, 20, 0, 0, 0, chinaStandardTime),
	}
	got := canonicalCustomer(customer)
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt.Location() != time.UTC ||
		!strings.Contains(string(body), `"created_at":"2026-07-24T12:00:00Z"`) {
		t.Fatalf("customer=%+v json=%s", got, body)
	}
}
