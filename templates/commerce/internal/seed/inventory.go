package seed

import (
	"fmt"
	"time"
)

// generateInventory populates locations, inventory levels, and the
// replenishment stock movements that justify each on_hand quantity. It runs in
// its own stream so adding a location field does not perturb order outcomes.
func generateInventory(ds *Dataset, c counts, seed int64, generatedAt time.Time) {
	stream := domainStream(seed, "inventory")

	// --- Locations: a small curated warehouse/store set.
	locationCount := c.Locations
	if locationCount > len(locationVocabulary) {
		locationCount = len(locationVocabulary)
	}
	ds.Locations = make([]LocationRow, 0, locationCount)
	for index := 0; index < locationCount; index++ {
		entry := locationVocabulary[index]
		ds.Locations = append(ds.Locations, LocationRow{
			LocationID: entry.ID,
			Name:       entry.Name,
			Type:       entry.Type,
			Priority:   index + 1,
		})
	}

	// --- Inventory levels: every SKU is stocked in the primary warehouse plus
	// ~50% of secondary locations. on_hand = sum of replenishment movements so
	// the verifier's movement/level reconciliation always holds.
	ds.InventoryLevels = make([]InventoryLevelRow, 0, len(ds.Variants)*locationCount)
	ds.StockMovements = make([]StockMovementRow, 0, len(ds.Variants)*locationCount)
	movementCounter := 0
	for _, variant := range ds.Variants {
		for li, location := range ds.Locations {
			// Primary warehouse always stocked; others 50% chance.
			if li > 0 && !boolP(stream, 0.50) {
				continue
			}
			// Two replenishment movements per level so the ledger reconciles
			// to on_hand exactly. Deltas are positive (replenishment).
			first := intRange(stream, 20, 80)
			second := intRange(stream, 20, 120)
			onHand := first + second
			movementCounter++
			firstID := fmt.Sprintf("mv-%08d", movementCounter)
			ds.StockMovements = append(ds.StockMovements, StockMovementRow{
				MovementID:  firstID,
				SKU:         variant.SKU,
				LocationID:  location.LocationID,
				Delta:       first,
				Reason:      "replenishment",
				ReferenceID: refPtr("replenish:" + firstID),
				CreatedAt:   generatedAt.Add(-time.Duration(intRange(stream, 1, 30)) * 24 * time.Hour),
			})
			movementCounter++
			secondID := fmt.Sprintf("mv-%08d", movementCounter)
			ds.StockMovements = append(ds.StockMovements, StockMovementRow{
				MovementID:  secondID,
				SKU:         variant.SKU,
				LocationID:  location.LocationID,
				Delta:       second,
				Reason:      "replenishment",
				ReferenceID: refPtr("replenish:" + secondID),
				CreatedAt:   generatedAt.Add(-time.Duration(intRange(stream, 1, 20)) * 24 * time.Hour),
			})
			ds.InventoryLevels = append(ds.InventoryLevels, InventoryLevelRow{
				SKU:        variant.SKU,
				LocationID: location.LocationID,
				OnHand:     onHand,
				Reserved:   0, // reservations are written by the order domain
				UpdatedAt:  generatedAt,
			})
		}
	}
}

var locationVocabulary = []struct {
	ID   string
	Name string
	Type string
}{
	{"wh-primary", "Primary Warehouse 主仓", "warehouse"},
	{"wh-west", "West Coast Warehouse 西岸仓", "warehouse"},
	{"wh-east", "East Coast Warehouse 东岸仓", "warehouse"},
	{"store-shanghai", "Shanghai Flagship 上海旗舰店", "store"},
	{"store-sf", "San Francisco Store 旧金山店", "store"},
	{"store-london", "London Store 伦敦店", "store"},
	{"wh-south", "South Warehouse 南仓", "warehouse"},
	{"wh-north", "North Warehouse 北仓", "warehouse"},
	{"store-bj", "Beijing Store 北京店", "store"},
	{"store-nyc", "New York Store 纽约店", "store"},
	{"wh-cn-south", "South China Warehouse 华南仓", "warehouse"},
	{"wh-cn-east", "East China Warehouse 华东仓", "warehouse"},
}

func refPtr(value string) *string { return &value }
