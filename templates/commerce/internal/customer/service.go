// Package customer owns customer reads and internal address validation.
package customer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

var (
	ErrCustomerNotFound = errors.New("customer not found")
	ErrAddressNotFound  = errors.New("customer address not found")
	ErrAddressNotUsable = errors.New("customer address not usable")
	errInvalidCursor    = errors.New("invalid customer cursor")
)

type Address struct {
	ID          string `json:"address_id"`
	CustomerID  string `json:"customer_id,omitempty"`
	Label       string `json:"label,omitempty"`
	Recipient   string `json:"recipient"`
	Phone       string `json:"phone"`
	CountryCode string `json:"country_code"`
	Province    string `json:"province"`
	City        string `json:"city"`
	District    string `json:"district"`
	Line1       string `json:"line1"`
	PostalCode  string `json:"postal_code"`
	Default     bool   `json:"is_default"`
}

type Customer struct {
	ID        string    `json:"customer_id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Phone     string    `json:"phone,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	Addresses []Address `json:"addresses,omitempty"`
}

type CustomerPage struct {
	Items      []Customer `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

type AddressValidation struct {
	CustomerID     string
	CustomerStatus string
	Address        Address
}

type ValidatedAddress struct {
	Valid    bool     `json:"valid"`
	Customer Customer `json:"customer"`
	Address  Address  `json:"address"`
}

type Store interface {
	ListCustomers(context.Context, string, int) ([]Customer, error)
	GetCustomer(context.Context, string) (Customer, error)
	GetAddressValidation(context.Context, string, string) (AddressValidation, error)
}

type Service struct{ store Store }

func NewService(store Store) *Service { return &Service{store: store} }

func (service *Service) ListCustomers(ctx context.Context, encodedCursor string, requestedSize int) (CustomerPage, error) {
	after, err := decodeCustomerCursor(encodedCursor)
	if err != nil {
		return CustomerPage{}, err
	}
	size := normalizeCustomerPageSize(requestedSize)
	customers, err := service.store.ListCustomers(ctx, after, size+1)
	if err != nil {
		return CustomerPage{}, fmt.Errorf("list customers: %w", err)
	}
	page := CustomerPage{Items: customers}
	if len(customers) > size {
		page.Items = customers[:size]
		page.NextCursor = encodeCustomerCursor(page.Items[len(page.Items)-1].ID)
	}
	if page.Items == nil {
		page.Items = []Customer{}
	}
	return page, nil
}

func (service *Service) GetCustomer(ctx context.Context, id string) (Customer, error) {
	if strings.TrimSpace(id) == "" {
		return Customer{}, ErrCustomerNotFound
	}
	return service.store.GetCustomer(ctx, id)
}

func (service *Service) ValidateAddress(ctx context.Context, customerID, addressID string) (ValidatedAddress, error) {
	if strings.TrimSpace(customerID) == "" || strings.TrimSpace(addressID) == "" {
		return ValidatedAddress{}, ErrAddressNotUsable
	}
	validation, err := service.store.GetAddressValidation(ctx, customerID, addressID)
	if err != nil {
		return ValidatedAddress{}, err
	}
	address := validation.Address
	if validation.CustomerID != customerID || address.CustomerID != customerID ||
		(validation.CustomerStatus != "active" && validation.CustomerStatus != "guest") ||
		!usableAddress(address) {
		return ValidatedAddress{}, ErrAddressNotUsable
	}
	return ValidatedAddress{
		Valid: true,
		Customer: Customer{
			ID: validation.CustomerID, Status: validation.CustomerStatus,
		},
		Address: address,
	}, nil
}

func usableAddress(address Address) bool {
	required := []string{
		address.ID, address.CustomerID, address.Recipient, address.Phone,
		address.CountryCode, address.Province, address.City, address.District,
		address.Line1, address.PostalCode,
	}
	for _, value := range required {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return len(address.CountryCode) == 2
}

type customerCursorEnvelope struct {
	Version int    `json:"v"`
	ID      string `json:"id"`
}

func encodeCustomerCursor(id string) string {
	body, _ := json.Marshal(customerCursorEnvelope{Version: 1, ID: id})
	return base64.RawURLEncoding.EncodeToString(body)
}

func decodeCustomerCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	body, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", errInvalidCursor
	}
	var envelope customerCursorEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Version != 1 ||
		envelope.ID == "" || strings.TrimSpace(envelope.ID) != envelope.ID {
		return "", errInvalidCursor
	}
	return envelope.ID, nil
}

func normalizeCustomerPageSize(requested int) int {
	if requested <= 0 {
		return DefaultPageSize
	}
	if requested > MaxPageSize {
		return MaxPageSize
	}
	return requested
}
