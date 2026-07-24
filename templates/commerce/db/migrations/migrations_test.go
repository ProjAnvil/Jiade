package migrations

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var serviceMigrations = map[string][]string{
	"catalog_db.sql": {
		"category", "brand", "product", "product_media", "product_option",
		"product_option_value", "variant", "variant_option_value", "price_list", "variant_price",
	},
	"customer_db.sql": {
		"membership_tier", "customer", "address", "customer_consent",
	},
	"inventory_db.sql": {
		"location", "inventory_level", "reservation", "stock_movement",
	},
	"order_db.sql": {
		"cart", "cart_item", "sales_order", "order_item", "order_discount_allocation",
		"order_status_history", "order_saga", "order_saga_step",
	},
	"payment_db.sql": {
		"payment_intent", "payment_method_snapshot", "payment_attempt", "refund", "webhook_inbox",
	},
	"fulfillment_db.sql": {
		"fulfillment_order", "fulfillment_item", "pick_item", "package", "package_item",
		"shipment", "tracking_event",
	},
}

func TestServiceMigrationsContainOwnedTablesAndSharedMessaging(t *testing.T) {
	for filename, domainTables := range serviceMigrations {
		t.Run(filename, func(t *testing.T) {
			migration := readMigration(t, filename)
			for _, table := range append(domainTables, "outbox_event", "inbox_event") {
				requirePattern(t, migration, `create\s+table\s+if\s+not\s+exists\s+`+regexp.QuoteMeta(table)+`\b`)
			}
			requirePattern(t, migration, `event_id\s+uuid\s+primary\s+key`)
			requirePattern(t, migration, `claim_token\s+uuid`)
			requirePattern(t, migration, `claimed_at\s+timestamptz`)
			requirePattern(t, migration, `attempts\s+integer`)
			requirePattern(t, migration, `published_at\s+timestamptz`)
			requirePattern(t, migration, `primary\s+key\s*\(\s*consumer\s*,\s*event_id\s*\)`)
			requirePattern(t, migration, `create\s+index\s+if\s+not\s+exists\s+\w*pending\w*[\s\S]*?where\s+published_at\s+is\s+null`)
			requirePattern(t, migration, `create\s+index\s+if\s+not\s+exists\s+\w*claim\w*[\s\S]*?claimed_at[\s\S]*?where\s+published_at\s+is\s+null`)
			if strings.Contains(migration, `\i`) {
				t.Fatal("psql include command is not executable by database/sql")
			}
		})
	}
}

func TestServiceMigrationDDLIsIdempotentByConstruction(t *testing.T) {
	createPattern := regexp.MustCompile(`(?m)\bcreate\s+(?:unique\s+)?(?:table|index)\s+[^\n(]+`)
	for filename := range serviceMigrations {
		t.Run(filename, func(t *testing.T) {
			migration := readMigration(t, filename)
			for _, statement := range createPattern.FindAllString(migration, -1) {
				if !strings.Contains(statement, " if not exists ") {
					t.Fatalf("CREATE TABLE/INDEX without IF NOT EXISTS: %q", statement)
				}
			}
			if regexp.MustCompile(`(?m)^\s*(drop|truncate|alter)\b`).MatchString(migration) {
				t.Fatal("migration contains destructive or repeat-sensitive DDL")
			}
		})
	}
}

func TestServiceMigrationsDeclareDomainConstraintsAndIndexes(t *testing.T) {
	requirements := map[string][]string{
		"catalog_db.sql": {
			`sku\s+text\s+primary\s+key`, `check\s*\(\s*price_minor\s*>=\s*0`,
			`idx_product_category`, `idx_variant_product`, `idx_variant_price_lookup`,
		},
		"customer_db.sql": {
			`idx_address_one_default[\s\S]*?\(\s*customer_id\s*\)\s*where\s+is_default`,
			`idx_customer_status`, `idx_address_customer`,
		},
		"inventory_db.sql": {
			`reserved\s*<=\s*on_hand`, `idx_reservation_active`,
			`where\s+status\s*=\s*'active'`, `idx_stock_movement_level`,
		},
		"order_db.sql": {
			`total_minor\s*=\s*subtotal_minor\s*-\s*discount_minor\s*\+\s*shipping_minor\s*\+\s*tax_minor`,
			`idx_order_customer`, `idx_cart_customer_status`, `idx_order_saga_state`,
		},
		"payment_db.sql": {
			`provider_reference\s+text\s+unique`, `provider_event_id\s+text\s+primary\s+key`,
			`amount_minor\s*>\s*0`, `idx_payment_order`,
		},
		"fulfillment_db.sql": {
			`tracking_number\s+text\s+not\s+null\s+unique`, `quantity\s*>\s*0`,
			`idx_fulfillment_order`, `idx_tracking_event_shipment`,
		},
	}
	for filename, patterns := range requirements {
		t.Run(filename, func(t *testing.T) {
			migration := readMigration(t, filename)
			for _, pattern := range patterns {
				requirePattern(t, migration, pattern)
			}
		})
	}
}

func readMigration(t *testing.T, filename string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Clean(filename))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ToLower(string(contents))
}

func requirePattern(t *testing.T, text, pattern string) {
	t.Helper()
	matched, err := regexp.MatchString(`(?s)`+pattern, text)
	if err != nil {
		t.Fatalf("invalid test pattern %q: %v", pattern, err)
	}
	if !matched {
		t.Fatalf("migration does not match %q", pattern)
	}
}
