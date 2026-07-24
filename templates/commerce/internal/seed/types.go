package seed

import (
	"encoding/json"
	"time"
)

// Dataset is the in-memory representation of a generated seed. Generate builds
// it deterministically from a Config; loaders stream it into PostgreSQL.
//
// Only dev/demo retain the full Dataset; the load scale streams rows directly
// through CopyFrom and never materialises the whole Dataset. The verifier
// operates on a Dataset so it can be unit-tested without a database.
type Dataset struct {
	Config Config

	Categories []CategoryRow
	Brands     []BrandRow
	Products   []ProductRow
	Variants   []VariantRow

	MembershipTiers []MembershipTierRow
	Customers       []CustomerRow
	Addresses       []AddressRow

	Locations       []LocationRow
	InventoryLevels []InventoryLevelRow
	StockMovements  []StockMovementRow

	Carts     []CartRow
	CartItems []CartItemRow

	Orders         []SalesOrderRow
	OrderItems     []OrderItemRow
	OrderTaxLines  []OrderTaxLineRow
	OrderSnapshots []OrderCustomerSnapshotRow
	Reservations   []ReservationRow

	PaymentIntents  []PaymentIntentRow
	PaymentAttempts []PaymentAttemptRow
	Refunds         []RefundRow

	Fulfillments     []FulfillmentOrderRow
	FulfillmentItems []FulfillmentItemRow
	Packages         []PackageRow
	Shipments        []ShipmentRow
	TrackingEvents   []TrackingEventRow
}

// --- Catalog domain --------------------------------------------------------

type CategoryRow struct {
	CategoryID string
	Name       string
	ParentID   *string
	Path       string
}

type BrandRow struct {
	BrandID   string
	Name      string
	Slug      string
	Status    string
	CreatedAt time.Time
}

type ProductRow struct {
	ProductID   string
	Title       string
	Description string
	Brand       string
	CategoryID  string
	Status      string
	CreatedAt   time.Time
}

type VariantRow struct {
	SKU            string
	ProductID      string
	Title          string
	Attributes     json.RawMessage
	Barcode        *string
	PriceMinor     int64
	CompareAtMinor *int64
	Currency       string
	WeightGrams    int
}

// --- Customer domain -------------------------------------------------------

type MembershipTierRow struct {
	TierID            string
	Name              string
	Rank              int
	MinimumSpendMinor int64
}

type CustomerRow struct {
	CustomerID string
	Email      string
	Name       string
	Phone      *string
	Status     string
	CreatedAt  time.Time
}

type AddressRow struct {
	AddressID   string
	CustomerID  string
	Label       string
	Recipient   string
	Phone       string
	CountryCode string
	Province    string
	City        string
	District    string
	Line1       string
	PostalCode  string
	IsDefault   bool
}

// --- Inventory domain ------------------------------------------------------

type LocationRow struct {
	LocationID string
	Name       string
	Type       string
	Priority   int
}

type InventoryLevelRow struct {
	SKU        string
	LocationID string
	OnHand     int
	Reserved   int
	UpdatedAt  time.Time
}

type StockMovementRow struct {
	MovementID  string
	SKU         string
	LocationID  string
	Delta       int
	Reason      string
	ReferenceID *string
	CreatedAt   time.Time
}

type ReservationRow struct {
	ReservationID  string
	OrderID        string
	SKU            string
	LocationID     string
	Quantity       int
	Status         string
	ExpiresAt      time.Time
	IdempotencyKey string
}

// --- Order domain ----------------------------------------------------------

type CartRow struct {
	CartID     string
	CustomerID string
	Status     string
	Currency   string
	ExpiresAt  time.Time
}

type CartItemRow struct {
	CartID         string
	SKU            string
	Quantity       int
	UnitPriceMinor int64
}

type SalesOrderRow struct {
	OrderID           string
	OrderNo           string
	CustomerID        string
	Status            string
	PaymentStatus     string
	FulfillmentStatus string
	Currency          string
	SubtotalMinor     int64
	DiscountMinor     int64
	ShippingMinor     int64
	TaxMinor          int64
	TotalMinor        int64
	ShippingAddress   json.RawMessage
	IdempotencyKey    string
	PlacedAt          time.Time
}

type OrderItemRow struct {
	OrderItemID    string
	OrderID        string
	SKU            string
	Title          string
	Quantity       int
	UnitPriceMinor int64
	DiscountMinor  int64
	TotalMinor     int64
}

type OrderTaxLineRow struct {
	TaxLineID       string
	OrderID         string
	OrderItemID     *string
	Jurisdiction    string
	RateBasisPoints int
	TaxableMinor    int64
	AmountMinor     int64
}

type OrderCustomerSnapshotRow struct {
	OrderID        string
	Email          string
	Name           string
	Phone          *string
	BillingAddress json.RawMessage
}

// --- Payment domain --------------------------------------------------------

type PaymentIntentRow struct {
	PaymentIntentID   string
	OrderID           string
	AmountMinor       int64
	Currency          string
	Status            string
	Provider          string
	ProviderReference *string
	IdempotencyKey    string
	CreatedAt         time.Time
}

type PaymentAttemptRow struct {
	AttemptID       string
	PaymentIntentID string
	Status          string
	FailureCode     *string
	AmountMinor     int64
	CreatedAt       time.Time
}

type RefundRow struct {
	RefundID        string
	PaymentIntentID string
	AmountMinor     int64
	Status          string
	Reason          string
	IdempotencyKey  string
	CreatedAt       time.Time
}

// --- Fulfillment domain ----------------------------------------------------

type FulfillmentOrderRow struct {
	FulfillmentID string
	OrderID       string
	LocationID    string
	Status        string
	CreatedAt     time.Time
}

type FulfillmentItemRow struct {
	FulfillmentID string
	OrderItemID   string
	SKU           string
	Quantity      int
}

type PackageRow struct {
	PackageID     string
	FulfillmentID string
	WeightGrams   int
	LengthMM      int
	WidthMM       int
	HeightMM      int
	CreatedAt     time.Time
}

type ShipmentRow struct {
	ShipmentID     string
	FulfillmentID  string
	Carrier        string
	TrackingNumber string
	Status         string
	ShippedAt      *time.Time
	DeliveredAt    *time.Time
}

type TrackingEventRow struct {
	TrackingEventID string
	ShipmentID      string
	Status          string
	Description     string
	Location        *string
	OccurredAt      time.Time
}
