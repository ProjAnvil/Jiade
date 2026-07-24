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
