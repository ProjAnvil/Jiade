package order

import (
	"errors"
	"math"
	"testing"
)

func TestCalculateTotalsInvariantUsesExactMinorUnits(t *testing.T) {
	got, err := CalculateTotals([]Line{
		{Quantity: 2, UnitPriceMinor: 999, DiscountMinor: 101},
		{Quantity: 1, UnitPriceMinor: 500, DiscountMinor: 50},
	}, 800, 125)
	if err != nil {
		t.Fatal(err)
	}
	want := Totals{
		Subtotal: 2498,
		Discount: 151,
		Shipping: 800,
		Tax:      125,
		Total:    3272,
	}
	if got != want {
		t.Fatalf("totals=%+v, want %+v", got, want)
	}
}

func TestCalculateTotalsInvariantRejectsInvalidMoney(t *testing.T) {
	tests := []struct {
		name     string
		lines    []Line
		shipping int64
		tax      int64
	}{
		{name: "empty order"},
		{name: "zero quantity", lines: []Line{{Quantity: 0, UnitPriceMinor: 100}}},
		{name: "negative quantity", lines: []Line{{Quantity: -1, UnitPriceMinor: 100}}},
		{name: "negative price", lines: []Line{{Quantity: 1, UnitPriceMinor: -1}}},
		{name: "negative discount", lines: []Line{{Quantity: 1, UnitPriceMinor: 100, DiscountMinor: -1}}},
		{name: "discount exceeds line", lines: []Line{{Quantity: 1, UnitPriceMinor: 100, DiscountMinor: 101}}},
		{name: "negative shipping", lines: []Line{{Quantity: 1, UnitPriceMinor: 100}}, shipping: -1},
		{name: "negative tax", lines: []Line{{Quantity: 1, UnitPriceMinor: 100}}, tax: -1},
		{name: "line multiplication overflow", lines: []Line{{Quantity: 2, UnitPriceMinor: math.MaxInt64}}},
		{name: "subtotal overflow", lines: []Line{
			{Quantity: 1, UnitPriceMinor: math.MaxInt64},
			{Quantity: 1, UnitPriceMinor: 1},
		}},
		{name: "total overflow", lines: []Line{{Quantity: 1, UnitPriceMinor: math.MaxInt64}}, shipping: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := CalculateTotals(test.lines, test.shipping, test.tax)
			if !errors.Is(err, ErrInvalidMoney) {
				t.Fatalf("error=%v, want ErrInvalidMoney", err)
			}
		})
	}
}

func TestAllocatePercentageDiscountUsesLargestRemainderDeterministically(t *testing.T) {
	got, err := AllocatePercentageDiscount([]int64{105, 105, 105}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{11, 10, 10}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("allocation=%v, want %v", got, want)
		}
	}
}

func TestRegionalTaxUsesDiscountedTaxableAmount(t *testing.T) {
	rate, tax, err := RegionalTax("CN", "上海市", 9000)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 1300 || tax != 1170 {
		t.Fatalf("rate=%d tax=%d, want 1300 and 1170", rate, tax)
	}
	if _, tax, err := RegionalTax("US", "OR", 9000); err != nil || tax != 0 {
		t.Fatalf("Oregon tax=%d err=%v, want zero", tax, err)
	}
}
