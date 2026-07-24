package seed

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"commerce/internal/order"
)

// generateOrders builds carts, sales orders, order items, tax lines, customer
// snapshots, and the inventory reservations each confirmed order produces.
// Money is derived with the order package's checked-arithmetic helpers so the
// order equation (total = subtotal - discount + shipping + tax) always holds.
func generateOrders(ds *Dataset, c counts, seed int64, generatedAt time.Time) error {
	stream := domainStream(seed, "orders")

	ds.Carts = make([]CartRow, 0, c.Orders)
	ds.CartItems = make([]CartItemRow, 0, c.Orders*3)
	ds.Orders = make([]SalesOrderRow, 0, c.Orders)
	ds.OrderItems = make([]OrderItemRow, 0, c.Orders*3)
	ds.OrderTaxLines = make([]OrderTaxLineRow, 0, c.Orders*3)
	ds.OrderSnapshots = make([]OrderCustomerSnapshotRow, 0, c.Orders)
	ds.Reservations = make([]ReservationRow, 0, c.Orders*3)

	if len(ds.Variants) == 0 || len(ds.Customers) == 0 || len(ds.Locations) == 0 {
		return errors.New("seed: orders require catalog, customers, and inventory to be generated first")
	}

	primaryLocation := ds.Locations[0].LocationID
	customerPool := len(ds.Customers)
	for orderIndex := 0; orderIndex < c.Orders; orderIndex++ {
		var customer CustomerRow
		if orderIndex > 5 && boolP(stream, 0.25) {
			customer = ds.Customers[stream.Intn(orderIndex)%customerPool]
		} else {
			customer = ds.Customers[orderIndex%customerPool]
		}
		bundle, err := generateOneOrder(stream, ds, orderIndex, customer, primaryLocation, generatedAt)
		if err != nil {
			return err
		}
		ds.Carts = append(ds.Carts, bundle.Cart)
		ds.Orders = append(ds.Orders, bundle.Order)
		ds.CartItems = append(ds.CartItems, bundle.CartItems...)
		ds.OrderItems = append(ds.OrderItems, bundle.OrderItems...)
		ds.OrderTaxLines = append(ds.OrderTaxLines, bundle.OrderTaxLines...)
		ds.Reservations = append(ds.Reservations, bundle.Reservations...)
		ds.OrderSnapshots = append(ds.OrderSnapshots, bundle.Snapshot)
	}

	// Adjust inventory reserved/committed to mirror the reservations we wrote.
	applyReservations(ds)
	return nil
}

// orderBundle is the set of rows generated for a single sales order. Both the
// batch (generateOrders) and streaming (generateOrdersStreaming) paths consume
// it so the per-order algorithm stays in exactly one place.
type orderBundle struct {
	Cart          CartRow
	Order         SalesOrderRow
	CartItems     []CartItemRow
	OrderItems    []OrderItemRow
	OrderTaxLines []OrderTaxLineRow
	Reservations  []ReservationRow
	Snapshot      OrderCustomerSnapshotRow
}

// generateOneOrder is the canonical per-order generator. It is the single
// source of truth for order money, items, tax lines, reservations, and the
// customer snapshot; the only stochastic input is the supplied stream.
func generateOneOrder(
	stream *rand.Rand,
	ds *Dataset,
	orderIndex int,
	customer CustomerRow,
	primaryLocation string,
	generatedAt time.Time,
) (orderBundle, error) {
	region := regionForCustomer(customer, ds)
	currency := region.Currency
	orderID := fmt.Sprintf("ord-%07d", orderIndex+1)
	orderNo := fmt.Sprintf("SO-%07d", orderIndex+1)
	cartID := fmt.Sprintf("cart-%07d", orderIndex+1)
	customerEmail := customer.Email
	customerName := customer.Name
	var customerPhone *string = customer.Phone

	cart := CartRow{
		CartID:     cartID,
		CustomerID: customer.CustomerID,
		Status:     "converted",
		Currency:   currency,
		ExpiresAt:  generatedAt.Add(24 * time.Hour),
	}

	lineCount := intRange(stream, 1, 4)
	lines := make([]order.Line, 0, lineCount)
	cartItems := make([]CartItemRow, 0, lineCount)
	itemRows := make([]OrderItemRow, 0, lineCount)
	taxLines := make([]OrderTaxLineRow, 0, lineCount)
	reservations := make([]ReservationRow, 0, lineCount)

	chosenSKUs := pickN(stream, len(ds.Variants), lineCount)
	itemCounter := 0
	var subtotal, discount int64
	for _, skuIdx := range chosenSKUs {
		variant := ds.Variants[skuIdx]
		quantity := int64(intRange(stream, 1, 3))
		unitPrice := variant.PriceMinor
		lineDiscount := int64(0)
		if boolP(stream, 0.30) {
			lineDiscount = (unitPrice * quantity * 5) / 100
		}
		lineGross := unitPrice * quantity
		lineTotal := lineGross - lineDiscount

		itemCounter++
		orderItemID := fmt.Sprintf("%s-i%02d", orderID, itemCounter)
		itemRows = append(itemRows, OrderItemRow{
			OrderItemID:    orderItemID,
			OrderID:        orderID,
			SKU:            variant.SKU,
			Title:          variant.Title,
			Quantity:       int(quantity),
			UnitPriceMinor: unitPrice,
			DiscountMinor:  lineDiscount,
			TotalMinor:     lineTotal,
		})
		lines = append(lines, order.Line{
			Quantity:       quantity,
			UnitPriceMinor: unitPrice,
			DiscountMinor:  lineDiscount,
		})
		cartItems = append(cartItems, CartItemRow{
			CartID:         cartID,
			SKU:            variant.SKU,
			Quantity:       int(quantity),
			UnitPriceMinor: unitPrice,
		})
		subtotal += lineGross
		discount += lineDiscount

		basisPoints, taxAmount, err := order.RegionalTax(region.Code, region.Region, lineTotal)
		if err != nil {
			return orderBundle{}, fmt.Errorf("seed: tax for order %s: %w", orderID, err)
		}
		taxLines = append(taxLines, OrderTaxLineRow{
			TaxLineID:       fmt.Sprintf("%s-t%02d", orderID, itemCounter),
			OrderID:         orderID,
			OrderItemID:     &orderItemID,
			Jurisdiction:    region.Code + "/" + region.Region,
			RateBasisPoints: int(basisPoints),
			TaxableMinor:    lineTotal,
			AmountMinor:     taxAmount,
		})

		reservations = append(reservations, ReservationRow{
			ReservationID:  fmt.Sprintf("%s-r%02d", orderID, itemCounter),
			OrderID:        orderID,
			SKU:            variant.SKU,
			LocationID:     primaryLocation,
			Quantity:       int(quantity),
			Status:         "committed",
			ExpiresAt:      generatedAt.Add(15 * time.Minute),
			IdempotencyKey: fmt.Sprintf("%s-reserve-%s", orderID, variant.SKU),
		})
	}

	shipping := shippingFor(currency, subtotal)
	var tax int64
	for _, tl := range taxLines {
		tax += tl.AmountMinor
	}
	totals, err := order.CalculateTotals(lines, shipping, tax)
	if err != nil {
		return orderBundle{}, fmt.Errorf("seed: totals for order %s: %w", orderID, err)
	}

	status, paymentStatus, fulfillmentStatus := orderLifecycle(stream)
	placedAt := generatedAt.Add(-time.Duration(intRange(stream, 0, 90)) * 24 * time.Hour)
	shippingAddress, _ := json.Marshal(map[string]any{
		"address_id": fmt.Sprintf("addr-%s-1", customer.CustomerID),
		"recipient":  customerName,
		"city":       region.City,
		"country":    region.Code,
	})

	order := SalesOrderRow{
		OrderID:           orderID,
		OrderNo:           orderNo,
		CustomerID:        customer.CustomerID,
		Status:            status,
		PaymentStatus:     paymentStatus,
		FulfillmentStatus: fulfillmentStatus,
		Currency:          currency,
		SubtotalMinor:     totals.Subtotal,
		DiscountMinor:     totals.Discount,
		ShippingMinor:     totals.Shipping,
		TaxMinor:          totals.Tax,
		TotalMinor:        totals.Total,
		ShippingAddress:   shippingAddress,
		IdempotencyKey:    fmt.Sprintf("checkout-%s", orderID),
		PlacedAt:          placedAt,
	}
	snapshot := OrderCustomerSnapshotRow{
		OrderID:        orderID,
		Email:          customerEmail,
		Name:           customerName,
		Phone:          customerPhone,
		BillingAddress: billingAddressJSON(region, customerName, customerPhone),
	}
	return orderBundle{
		Cart:          cart,
		Order:         order,
		CartItems:     cartItems,
		OrderItems:    itemRows,
		OrderTaxLines: taxLines,
		Reservations:  reservations,
		Snapshot:      snapshot,
	}, nil
}

// orderLifecycle returns a (status, payment_status, fulfillment_status) triple
// that satisfies every CHECK in sales_order. The roll is the only stochastic
// input so the triple distribution stays stable across runs.
func orderLifecycle(stream *rand.Rand) (string, string, string) {
	roll := stream.Float64()
	switch {
	case roll < 0.55:
		// Happy path: confirmed -> paid -> fulfilled (but not yet completed).
		return "completed", "paid", "fulfilled"
	case roll < 0.70:
		// Confirmed, paid, partial fulfilment in flight.
		return "confirmed", "paid", "partial"
	case roll < 0.80:
		// Confirmed, paid, not yet shipped.
		return "confirmed", "paid", "unfulfilled"
	case roll < 0.90:
		// Cancelled before capture: payment_status=failed is the only way to
		// combine status=cancelled with a non-paid payment per the matrix.
		return "cancelled", "failed", "unfulfilled"
	default:
		// Refunded after fulfilment.
		return "completed", "refunded", "fulfilled"
	}
}

func shippingFor(currency string, subtotal int64) int64 {
	switch currency {
	case "CNY":
		base := int64(1500) // 15.00
		if subtotal > 100000 {
			base = 0 // free shipping above 1000.00
		}
		return base
	case "GBP":
		base := int64(495)
		if subtotal > 50000 {
			base = 0
		}
		return base
	default: // USD
		base := int64(595)
		if subtotal > 7500 {
			base = 0
		}
		return base
	}
}

func regionForCustomer(customer CustomerRow, ds *Dataset) customerRegionLike {
	// Look up the default address to recover the region.
	for _, addr := range ds.Addresses {
		if addr.CustomerID == customer.CustomerID && addr.IsDefault {
			for _, region := range customerRegions {
				if region.Code == addr.CountryCode && region.Region == addr.Province {
					return customerRegionLikeFromVocab(region)
				}
			}
			// Fall back to country match if province text drifted.
			for _, region := range customerRegions {
				if region.Code == addr.CountryCode {
					return customerRegionLikeFromVocab(region)
				}
			}
		}
	}
	// Defensive default: US.
	return customerRegionLikeFromVocab(customerRegions[3])
}

type customerRegionLike struct {
	Country, Code, Region, City, District, Currency string
}

// customerRegionLikeFromVocab builds a customerRegionLike from the vocabulary
// tuple shape without exporting it.
func customerRegionLikeFromVocab(v struct {
	Country, Code, Region, City, District, Currency string
}) customerRegionLike {
	return customerRegionLike{
		Country: v.Country, Code: v.Code, Region: v.Region,
		City: v.City, District: v.District, Currency: v.Currency,
	}
}

func billingAddressJSON(region customerRegionLike, recipient string, phone *string) json.RawMessage {
	phoneStr := ""
	if phone != nil {
		phoneStr = *phone
	}
	body, _ := json.Marshal(map[string]any{
		"recipient": recipient,
		"phone":     phoneStr,
		"city":      region.City,
		"country":   region.Code,
	})
	return body
}

// applyReservations folds the committed reservations into inventory_level so
// reserved <= on_hand holds for every level and the verifier's
// movement/level reconciliation stays sound.
func applyReservations(ds *Dataset) {
	reservedByLevel := make(map[string]int)
	for _, reservation := range ds.Reservations {
		if reservation.Status != "committed" && reservation.Status != "active" {
			continue
		}
		key := reservation.SKU + "|" + reservation.LocationID
		reservedByLevel[key] += reservation.Quantity
	}
	for index := range ds.InventoryLevels {
		level := &ds.InventoryLevels[index]
		key := level.SKU + "|" + level.LocationID
		if reserved := reservedByLevel[key]; reserved > 0 {
			// Never let reserved exceed on_hand; the generator picked ample
			// replenishment so this branch usually only ticks reserved up.
			if reserved > level.OnHand {
				level.OnHand = reserved
			}
			level.Reserved = reserved
		}
	}
}
