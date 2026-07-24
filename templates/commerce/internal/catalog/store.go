package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func (store *PostgresStore) ListProducts(ctx context.Context, after string, limit int) ([]Product, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT p.product_id, p.title, p.brand, c.name, p.status
		FROM product p
		JOIN category c USING (category_id)
		WHERE p.product_id > $1
		ORDER BY p.product_id
		LIMIT $2`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("query products: %w", err)
	}
	defer rows.Close()
	products := make([]Product, 0, limit)
	for rows.Next() {
		var product Product
		if err := rows.Scan(&product.ID, &product.Title, &product.Brand, &product.Category, &product.Status); err != nil {
			return nil, fmt.Errorf("scan product: %w", err)
		}
		products = append(products, product)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate products: %w", err)
	}
	return products, nil
}

func (store *PostgresStore) GetProduct(ctx context.Context, id string) (Product, error) {
	var product Product
	err := store.pool.QueryRow(ctx, `
		SELECT p.product_id, p.title, p.description, p.brand, c.name, p.status
		FROM product p
		JOIN category c USING (category_id)
		WHERE p.product_id = $1`, id).
		Scan(&product.ID, &product.Title, &product.Description, &product.Brand, &product.Category, &product.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Product{}, ErrProductNotFound
	}
	if err != nil {
		return Product{}, fmt.Errorf("query product: %w", err)
	}
	rows, err := store.pool.Query(ctx, `
		SELECT sku, title, attributes, COALESCE(barcode, ''), price_minor,
		       compare_at_minor, currency, weight_grams
		FROM variant
		WHERE product_id = $1
		ORDER BY sku`, id)
	if err != nil {
		return Product{}, fmt.Errorf("query variants: %w", err)
	}
	defer rows.Close()
	product.Variants = []Variant{}
	for rows.Next() {
		var variant Variant
		var attributes []byte
		if err := rows.Scan(&variant.SKU, &variant.Title, &attributes, &variant.Barcode,
			&variant.PriceMinor, &variant.CompareAtMinor, &variant.Currency, &variant.WeightGrams); err != nil {
			return Product{}, fmt.Errorf("scan variant: %w", err)
		}
		if err := json.Unmarshal(attributes, &variant.Attributes); err != nil {
			return Product{}, fmt.Errorf("decode variant attributes: %w", err)
		}
		product.Variants = append(product.Variants, variant)
	}
	if err := rows.Err(); err != nil {
		return Product{}, fmt.Errorf("iterate variants: %w", err)
	}
	return product, nil
}

func (store *PostgresStore) GetCheckoutSnapshot(ctx context.Context, sku string) (CheckoutSnapshot, error) {
	var snapshot CheckoutSnapshot
	var attributes []byte
	err := store.pool.QueryRow(ctx, `
		SELECT p.product_id, v.sku, p.title, v.title, p.status,
		       v.price_minor, v.currency, v.weight_grams, v.attributes
		FROM variant v
		JOIN product p ON p.product_id = v.product_id
		WHERE v.sku = $1`, sku).
		Scan(&snapshot.ProductID, &snapshot.SKU, &snapshot.ProductTitle,
			&snapshot.VariantTitle, &snapshot.Status, &snapshot.UnitPriceMinor,
			&snapshot.Currency, &snapshot.WeightGrams, &attributes)
	if errors.Is(err, pgx.ErrNoRows) {
		return CheckoutSnapshot{}, ErrSKUNotFound
	}
	if err != nil {
		return CheckoutSnapshot{}, fmt.Errorf("query checkout SKU snapshot: %w", err)
	}
	if err := json.Unmarshal(attributes, &snapshot.Attributes); err != nil {
		return CheckoutSnapshot{}, fmt.Errorf("decode checkout SKU attributes: %w", err)
	}
	snapshot.Title = snapshot.ProductTitle + " — " + snapshot.VariantTitle
	return snapshot, nil
}

var _ Store = (*PostgresStore)(nil)
