// Package catalog owns product reads and catalog HTTP contracts.
package catalog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

var ErrProductNotFound = errors.New("catalog product not found")

type Variant struct {
	SKU            string         `json:"sku"`
	Title          string         `json:"title"`
	Attributes     map[string]any `json:"attributes,omitempty"`
	Barcode        string         `json:"barcode,omitempty"`
	PriceMinor     int64          `json:"price_minor"`
	CompareAtMinor *int64         `json:"compare_at_minor,omitempty"`
	Currency       string         `json:"currency"`
	WeightGrams    int            `json:"weight_grams,omitempty"`
}

type Product struct {
	ID          string    `json:"product_id"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Brand       string    `json:"brand"`
	Category    string    `json:"category"`
	Status      string    `json:"status"`
	Variants    []Variant `json:"variants,omitempty"`
}

type ProductPage struct {
	Items      []Product `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

type Store interface {
	ListProducts(context.Context, string, int) ([]Product, error)
	GetProduct(context.Context, string) (Product, error)
}

type Service struct{ store Store }

func NewService(store Store) *Service { return &Service{store: store} }

func (service *Service) ListProducts(ctx context.Context, encodedCursor string, requestedSize int) (ProductPage, error) {
	after, err := decodeCursor(encodedCursor)
	if err != nil {
		return ProductPage{}, err
	}
	size := normalizePageSize(requestedSize)
	products, err := service.store.ListProducts(ctx, after, size+1)
	if err != nil {
		return ProductPage{}, fmt.Errorf("list catalog products: %w", err)
	}
	page := ProductPage{Items: products}
	if len(products) > size {
		page.Items = products[:size]
		page.NextCursor = encodeCursor(page.Items[len(page.Items)-1].ID)
	}
	if page.Items == nil {
		page.Items = []Product{}
	}
	return page, nil
}

func (service *Service) GetProduct(ctx context.Context, id string) (Product, error) {
	if strings.TrimSpace(id) == "" {
		return Product{}, ErrProductNotFound
	}
	return service.store.GetProduct(ctx, id)
}

type cursorEnvelope struct {
	Version int    `json:"v"`
	ID      string `json:"id"`
}

var errInvalidCursor = errors.New("invalid catalog cursor")

func encodeCursor(id string) string {
	body, _ := json.Marshal(cursorEnvelope{Version: 1, ID: id})
	return base64.RawURLEncoding.EncodeToString(body)
}

func decodeCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	body, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", errInvalidCursor
	}
	var envelope cursorEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Version != 1 ||
		envelope.ID == "" || strings.TrimSpace(envelope.ID) != envelope.ID {
		return "", errInvalidCursor
	}
	return envelope.ID, nil
}

func normalizePageSize(requested int) int {
	if requested <= 0 {
		return DefaultPageSize
	}
	if requested > MaxPageSize {
		return MaxPageSize
	}
	return requested
}
