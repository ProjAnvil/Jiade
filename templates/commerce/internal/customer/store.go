package customer

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func (store *PostgresStore) ListCustomers(ctx context.Context, after string, limit int) ([]Customer, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT customer_id, email, name, COALESCE(phone, ''), status, created_at
		FROM customer
		WHERE customer_id > $1
		ORDER BY customer_id
		LIMIT $2`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("query customers: %w", err)
	}
	defer rows.Close()
	customers := make([]Customer, 0, limit)
	for rows.Next() {
		var customer Customer
		if err := rows.Scan(&customer.ID, &customer.Email, &customer.Name, &customer.Phone,
			&customer.Status, &customer.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan customer: %w", err)
		}
		customers = append(customers, customer)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate customers: %w", err)
	}
	return customers, nil
}

func (store *PostgresStore) GetCustomer(ctx context.Context, id string) (Customer, error) {
	var customer Customer
	err := store.pool.QueryRow(ctx, `
		SELECT customer_id, email, name, COALESCE(phone, ''), status, created_at
		FROM customer
		WHERE customer_id = $1`, id).
		Scan(&customer.ID, &customer.Email, &customer.Name, &customer.Phone,
			&customer.Status, &customer.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Customer{}, ErrCustomerNotFound
	}
	if err != nil {
		return Customer{}, fmt.Errorf("query customer: %w", err)
	}
	addresses, err := store.addresses(ctx, id)
	if err != nil {
		return Customer{}, err
	}
	customer.Addresses = addresses
	return customer, nil
}

func (store *PostgresStore) addresses(ctx context.Context, customerID string) ([]Address, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT address_id, customer_id, label, recipient, phone, country_code,
		       province, city, district, line1, postal_code, is_default
		FROM address
		WHERE customer_id = $1
		ORDER BY address_id`, customerID)
	if err != nil {
		return nil, fmt.Errorf("query customer addresses: %w", err)
	}
	defer rows.Close()
	addresses := []Address{}
	for rows.Next() {
		var address Address
		if err := rows.Scan(&address.ID, &address.CustomerID, &address.Label, &address.Recipient,
			&address.Phone, &address.CountryCode, &address.Province, &address.City,
			&address.District, &address.Line1, &address.PostalCode, &address.Default); err != nil {
			return nil, fmt.Errorf("scan customer address: %w", err)
		}
		addresses = append(addresses, address)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate customer addresses: %w", err)
	}
	return addresses, nil
}

func (store *PostgresStore) GetAddressValidation(ctx context.Context, customerID, addressID string) (AddressValidation, error) {
	var validation AddressValidation
	validation.CustomerID = customerID
	err := store.pool.QueryRow(ctx, `
		SELECT c.status, a.address_id, a.customer_id, a.label, a.recipient, a.phone,
		       a.country_code, a.province, a.city, a.district, a.line1,
		       a.postal_code, a.is_default
		FROM customer c
		JOIN address a ON a.customer_id = c.customer_id
		WHERE c.customer_id = $1 AND a.address_id = $2`, customerID, addressID).
		Scan(&validation.CustomerStatus, &validation.Address.ID, &validation.Address.CustomerID,
			&validation.Address.Label, &validation.Address.Recipient, &validation.Address.Phone,
			&validation.Address.CountryCode, &validation.Address.Province, &validation.Address.City,
			&validation.Address.District, &validation.Address.Line1, &validation.Address.PostalCode,
			&validation.Address.Default)
	if errors.Is(err, pgx.ErrNoRows) {
		return AddressValidation{}, ErrAddressNotFound
	}
	if err != nil {
		return AddressValidation{}, fmt.Errorf("query customer address validation: %w", err)
	}
	return validation, nil
}

var _ Store = (*PostgresStore)(nil)
