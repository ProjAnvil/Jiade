package seed

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
)

func TestPickHandlesEmptyVocabulary(t *testing.T) {
	stream := rand.New(rand.NewSource(1))
	if got := pick(stream, []string{}); got != "" {
		t.Fatalf("pick(empty)=%q, want zero value", got)
	}
	vocabulary := []string{"a", "b", "c"}
	for index := 0; index < 20; index++ {
		if got := pick(stream, vocabulary); !strings.Contains("abc", got) {
			t.Fatalf("pick=%q outside vocabulary", got)
		}
	}
}

func TestPickNRejectsImpossibleCounts(t *testing.T) {
	stream := rand.New(rand.NewSource(1))
	defer func() {
		if recover() == nil {
			t.Error("pickN with n>size must panic")
		}
	}()
	pickN(stream, 2, 3)
}

func TestRangeHelpersSwapAndClamp(t *testing.T) {
	stream := rand.New(rand.NewSource(11))
	for index := 0; index < 50; index++ {
		if got := intRange(stream, 10, 2); got < 2 || got > 10 {
			t.Fatalf("intRange(swapped)=%d out of [2,10]", got)
		}
		if got := int64Range(stream, int64(20), int64(5)); got < 5 || got > 20 {
			t.Fatalf("int64Range(swapped)=%d out of [5,20]", got)
		}
		if got := intRange(stream, 4, 4); got != 4 {
			t.Fatalf("intRange(4,4)=%d, want 4", got)
		}
	}
}

func TestPostalForShapesPerCountry(t *testing.T) {
	stream := rand.New(rand.NewSource(5))
	// CN postal codes start with 2 and carry at least five digits.
	if got := postalFor("CN", stream); !strings.HasPrefix(got, "2") || len(got) < 5 {
		t.Fatalf("CN postal=%q, want 2-prefixed five-plus digits", got)
	}
	// US postal codes are five digits.
	if got := postalFor("US", stream); len(got) != 5 {
		t.Fatalf("US postal=%q, want 5 digits", got)
	}
	// GB uses the SW1A nAA shape.
	if got := postalFor("GB", stream); !strings.HasPrefix(got, "SW1A ") || !strings.HasSuffix(got, "AA") {
		t.Fatalf("GB postal=%q, want SW1A nAA", got)
	}
	// Unknown countries fall back to the generic five-digit shape.
	if got := postalFor("JP", stream); len(got) != 5 {
		t.Fatalf("default postal=%q, want 5 digits", got)
	}
}

func TestRegionForCustomerFallsBackGracefully(t *testing.T) {
	empty := Dataset{}
	// No addresses at all: the defensive US default applies.
	if got := regionForCustomer(CustomerRow{CustomerID: "CUS-1"}, &empty); got.Code != "US" {
		t.Fatalf("empty dataset region=%+v, want US default", got)
	}

	// A default address whose country+province match the vocabulary resolves
	// to that region rather than the fallback.
	vocabByCode := make(map[string]customerRegionLike)
	for _, region := range customerRegions {
		vocabByCode[region.Code] = customerRegionLikeFromVocab(region)
	}
	anyCode := customerRegions[0].Code
	matched := Dataset{Addresses: []AddressRow{{
		CustomerID: "CUS-2", CountryCode: anyCode,
		Province: customerRegions[0].Region, IsDefault: true,
	}}}
	if got := regionForCustomer(CustomerRow{CustomerID: "CUS-2"}, &matched); got.Code != anyCode {
		t.Fatalf("matched region=%+v, want %s", got, anyCode)
	}

	// A default address with a known country but drifted province falls back
	// to the country match.
	drifted := Dataset{Addresses: []AddressRow{{
		CustomerID: "CUS-3", CountryCode: anyCode, Province: "Nowhere", IsDefault: true,
	}}}
	if got := regionForCustomer(CustomerRow{CustomerID: "CUS-3"}, &drifted); got.Code != anyCode {
		t.Fatalf("country fallback region=%+v, want %s", got, anyCode)
	}

	// A non-default address is ignored.
	nonDefault := Dataset{Addresses: []AddressRow{{
		CustomerID: "CUS-4", CountryCode: anyCode, Province: customerRegions[0].Region,
	}}}
	if got := regionForCustomer(CustomerRow{CustomerID: "CUS-4"}, &nonDefault); got.Code != "US" {
		t.Fatalf("non-default address region=%+v, want US fallback", got)
	}
}

func TestViolationErrorFormatting(t *testing.T) {
	// The verifier's Violation carries the sentinel code and a human message.
	cause := errors.New("order_equation")
	violation := Violation{Code: cause, Message: "ORD-1 total mismatch"}
	if !strings.Contains(violation.Error(), "order_equation") ||
		!strings.Contains(violation.Error(), "ORD-1 total mismatch") {
		t.Fatalf("Error()=%q, want code and message", violation.Error())
	}
	if !errors.Is(violation, cause) {
		t.Fatal("Violation must unwrap to its sentinel code")
	}
}
