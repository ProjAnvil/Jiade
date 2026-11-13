package seed

import (
	"fmt"
	"math/rand"
	"time"
)

// generateFulfillment builds one fulfilment per shipped order plus packages,
// shipments, and tracking histories. Cancelled/unfulfilled orders produce no
// fulfilment row (the schema allows status='unfulfilled' with no fulfilment
// child).
func generateFulfillment(ds *Dataset, c counts, seed int64, generatedAt time.Time) {
	stream := domainStream(seed, "fulfillment")

	ds.Fulfillments = make([]FulfillmentOrderRow, 0, len(ds.Orders))
	ds.FulfillmentItems = make([]FulfillmentItemRow, 0, len(ds.OrderItems))
	ds.Packages = make([]PackageRow, 0, len(ds.Orders))
	ds.Shipments = make([]ShipmentRow, 0, len(ds.Orders))
	ds.TrackingEvents = make([]TrackingEventRow, 0, len(ds.Orders)*4)

	primaryLocation := ds.Locations[0].LocationID
	for _, order := range ds.Orders {
		items := orderItemsFor(ds, order.OrderID)
		ff, fulfilItems, pkg, shipment, events := generateOneFulfillment(stream, order, items, generatedAt)
		if ff == nil {
			continue
		}
		ff.LocationID = primaryLocation
		ds.Fulfillments = append(ds.Fulfillments, *ff)
		ds.FulfillmentItems = append(ds.FulfillmentItems, fulfilItems...)
		ds.Packages = append(ds.Packages, pkg)
		ds.Shipments = append(ds.Shipments, shipment)
		ds.TrackingEvents = append(ds.TrackingEvents, events...)
	}
}

// orderItemsFor returns the order items for a given order ID. Linear scan is
// fine for dev/demo; the streaming path passes items directly.
func orderItemsFor(ds *Dataset, orderID string) []OrderItemRow {
	var out []OrderItemRow
	for _, item := range ds.OrderItems {
		if item.OrderID == orderID {
			out = append(out, item)
		}
	}
	return out
}

// generateOneFulfillment is the canonical per-order fulfilment generator. It
// returns nil ff for unfulfilled orders. Both the batch (generateFulfillment)
// and streaming (generateFulfillmentStreaming) paths share it.
func generateOneFulfillment(
	stream *rand.Rand,
	order SalesOrderRow,
	items []OrderItemRow,
	generatedAt time.Time,
) (*FulfillmentOrderRow, []FulfillmentItemRow, PackageRow, ShipmentRow, []TrackingEventRow) {
	if order.FulfillmentStatus == "unfulfilled" {
		return nil, nil, PackageRow{}, ShipmentRow{}, nil
	}
	fulfilStatus := "fulfilled"
	if order.FulfillmentStatus == "partial" {
		fulfilStatus = "in_progress"
	}
	fulfilID := fmt.Sprintf("%s-ff", order.OrderID)
	// locationID is not known to this helper; the caller fills it in via the
	// streaming emitter. For the batch path the caller passes ds.Locations[0]
	// before emit, so we default to empty and let the caller overwrite.
	ff := &FulfillmentOrderRow{
		FulfillmentID: fulfilID,
		OrderID:       order.OrderID,
		LocationID:    "",
		Status:        fulfilStatus,
		CreatedAt:     order.PlacedAt.Add(time.Minute),
	}
	var fulfilItems []FulfillmentItemRow
	for _, item := range items {
		fulfilItems = append(fulfilItems, FulfillmentItemRow{
			FulfillmentID: fulfilID,
			OrderItemID:   item.OrderItemID,
			SKU:           item.SKU,
			Quantity:      item.Quantity,
		})
	}

	weight := intRange(stream, 200, 5000)
	packageID := fmt.Sprintf("%s-pkg1", order.OrderID)
	pkg := PackageRow{
		PackageID:     packageID,
		FulfillmentID: fulfilID,
		WeightGrams:   weight,
		LengthMM:      intRange(stream, 100, 600),
		WidthMM:       intRange(stream, 100, 400),
		HeightMM:      intRange(stream, 50, 300),
		CreatedAt:     order.PlacedAt.Add(2 * time.Minute),
	}

	carrier := pick(stream, carriers)
	trackingNumber := fmt.Sprintf("TRK%s%d", carrierCode(carrier), int64Range(stream, 100000, 999999))
	shipStatus, shippedAt, deliveredAt := shipmentStatus(stream, fulfilStatus, order.PlacedAt)
	shipment := ShipmentRow{
		ShipmentID:     fmt.Sprintf("%s-shp1", order.OrderID),
		FulfillmentID:  fulfilID,
		Carrier:        carrier,
		TrackingNumber: trackingNumber,
		Status:         shipStatus,
		ShippedAt:      shippedAt,
		DeliveredAt:    deliveredAt,
	}
	shipmentID := fmt.Sprintf("%s-shp1", order.OrderID)
	drafts := trackingHistory(stream, shipStatus, shippedAt, deliveredAt, order.PlacedAt)
	events := make([]TrackingEventRow, 0, len(drafts))
	for ei, ev := range drafts {
		events = append(events, TrackingEventRow{
			TrackingEventID: fmt.Sprintf("%s-te%d", shipmentID, ei+1),
			ShipmentID:      shipmentID,
			Status:          ev.Status,
			Description:     ev.Description,
			Location:        ev.Location,
			OccurredAt:      ev.OccurredAt,
		})
	}
	return ff, fulfilItems, pkg, shipment, events
}

type trackingEventDraft struct {
	Status      string
	Description string
	Location    *string
	OccurredAt  time.Time
}

func trackingHistory(stream *rand.Rand, finalStatus string, shippedAt, deliveredAt *time.Time, placedAt time.Time) []trackingEventDraft {
	var events []trackingEventDraft
	start := placedAt.Add(time.Hour)
	if shippedAt != nil {
		start = *shippedAt
	}
	loc1 := locPtr("Origin Sort Center 始发分拣中心")
	loc2 := locPtr("In Transit Hub 中转中心")
	loc3 := locPtr("Destination Hub 目的地")
	loc4 := locPtr("Final Delivery 末端配送")
	events = append(events, trackingEventDraft{
		Status: "picked_up", Description: "Package picked up 包裹已揽收",
		Location: loc1, OccurredAt: start,
	})
	events = append(events, trackingEventDraft{
		Status: "in_transit", Description: "In transit 运输中",
		Location: loc2, OccurredAt: start.Add(12 * time.Hour),
	})
	if finalStatus == "delivered" && deliveredAt != nil {
		events = append(events, trackingEventDraft{
			Status: "out_for_delivery", Description: "Out for delivery 派送中",
			Location: loc3, OccurredAt: deliveredAt.Add(-6 * time.Hour),
		})
		events = append(events, trackingEventDraft{
			Status: "delivered", Description: "Delivered 已送达",
			Location: loc4, OccurredAt: *deliveredAt,
		})
	} else if finalStatus == "delayed" {
		events = append(events, trackingEventDraft{
			Status: "delayed", Description: "Delayed in transit 运输延误",
			Location: loc3, OccurredAt: start.Add(36 * time.Hour),
		})
	} else {
		// in_transit / label_created: keep history short.
	}
	// Cap to 4 events for stable dev summary shape.
	if len(events) > 4 {
		events = events[:4]
	}
	return events
}

func shipmentStatus(stream *rand.Rand, fulfilStatus string, placedAt time.Time) (string, *time.Time, *time.Time) {
	switch fulfilStatus {
	case "fulfilled":
		shipped := placedAt.Add(24 * time.Hour)
		delivered := placedAt.Add(72 * time.Hour)
		return "delivered", &shipped, &delivered
	case "in_progress":
		roll := stream.Float64()
		shipped := placedAt.Add(24 * time.Hour)
		if roll < 0.7 {
			return "in_transit", &shipped, nil
		}
		return "delayed", &shipped, nil
	default:
		return "label_created", nil, nil
	}
}

func carrierCode(carrier string) string {
	switch {
	case contains(carrier, "SF"):
		return "SF"
	case contains(carrier, "UPS"):
		return "1Z"
	case contains(carrier, "DHL"):
		return "DHL"
	case contains(carrier, "FedEx"):
		return "FDX"
	case contains(carrier, "Royal"):
		return "RM"
	default:
		return "CP"
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func locPtr(value string) *string { return &value }
