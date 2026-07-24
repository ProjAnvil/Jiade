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
			for _, statement := range regexp.MustCompile(`(?m)^\s*drop\b[^\n;]*`).FindAllString(migration, -1) {
				if !regexp.MustCompile(`^\s*drop\s+trigger\s+if\s+exists\b`).MatchString(statement) {
					t.Fatalf("migration contains unsafe DROP statement: %q", statement)
				}
			}
			if regexp.MustCompile(`(?m)^\s*(truncate|alter)\b`).MatchString(migration) {
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
			`available\s+integer\s+generated\s+always\s+as\s*\(\s*on_hand\s*-\s*reserved\s*\)\s+stored`,
		},
		"order_db.sql": {
			`total_minor\s*=\s*subtotal_minor\s*-\s*discount_minor\s*\+\s*shipping_minor\s*\+\s*tax_minor`,
			`unique\s*\(\s*order_id\s*,\s*order_item_id\s*\)`,
			`foreign\s+key\s*\(\s*order_id\s*,\s*order_item_id\s*\)[\s\S]*?references\s+order_item\s*\(\s*order_id\s*,\s*order_item_id\s*\)`,
			`create\s+unique\s+index\s+if\s+not\s+exists\s+idx_order_discount_order_key[\s\S]*?\(\s*order_id\s*,\s*source\s*\)[\s\S]*?where\s+order_item_id\s+is\s+null`,
			`create\s+unique\s+index\s+if\s+not\s+exists\s+idx_order_discount_line_key[\s\S]*?\(\s*order_id\s*,\s*order_item_id\s*,\s*source\s*\)[\s\S]*?where\s+order_item_id\s+is\s+not\s+null`,
			`status\s*<>\s*'completed'[\s\S]*?fulfillment_status\s*=\s*'fulfilled'`,
			`payment_status\s*<>\s*'failed'\s+or\s+status\s*=\s*'cancelled'`,
			`fulfillment_status\s*=\s*'unfulfilled'[\s\S]*?status\s+in\s*\(\s*'confirmed'\s*,\s*'completed'\s*,\s*'cancelled'\s*\)`,
			`idx_order_placed_at[\s\S]*?on\s+sales_order\s*\(\s*placed_at\s+desc`,
			`idx_order_customer`, `idx_cart_customer_status`, `idx_order_saga_state`,
		},
		"payment_db.sql": {
			`provider_reference\s+text\s+unique`, `provider_event_id\s+text\s+primary\s+key`,
			`amount_minor\s*>\s*0`, `idx_payment_order`,
			`create\s+or\s+replace\s+function\s+validate_payment_attempt`,
			`new\.amount_minor\s*>\s+intent_amount`,
			`new\.status\s*=\s*'failed'[\s\S]*?new\.failure_code\s+is\s+null`,
			`drop\s+trigger\s+if\s+exists\s+trg_validate_payment_attempt`,
			`create\s+trigger\s+trg_validate_payment_attempt`,
			`create\s+or\s+replace\s+function\s+validate_refund`,
			`for\s+update`,
			`coalesce\s*\(\s*sum\s*\(\s*amount_minor\s*\)`,
			`status\s+in\s*\(\s*'pending'\s*,\s*'succeeded'\s*\)`,
			`drop\s+trigger\s+if\s+exists\s+trg_validate_refund`,
			`create\s+trigger\s+trg_validate_refund`,
			`create\s+or\s+replace\s+function\s+validate_payment_intent_update`,
			`new\.amount_minor\s*<>\s*old\.amount_minor`,
			`exists\s*\([\s\S]*?from\s+payment_attempt`,
			`exists\s*\([\s\S]*?from\s+refund`,
			`old\.status\s+in\s*\(\s*'succeeded'\s*,\s*'partially_refunded'\s*,\s*'refunded'\s*\)`,
			`new\.status\s+not\s+in\s*\(\s*'succeeded'\s*,\s*'partially_refunded'\s*,\s*'refunded'\s*\)`,
			`drop\s+trigger\s+if\s+exists\s+trg_validate_payment_intent_update`,
			`create\s+trigger\s+trg_validate_payment_intent_update`,
			`idx_payment_created_at[\s\S]*?on\s+payment_intent\s*\(\s*created_at\s+desc`,
		},
		"fulfillment_db.sql": {
			`tracking_number\s+text\s+not\s+null\s+unique`, `quantity\s*>\s*0`,
			`status\s*=\s*'delivered'[\s\S]*?delivered_at\s+is\s+not\s+null`,
			`idx_fulfillment_created_at[\s\S]*?on\s+fulfillment_order\s*\(\s*created_at\s+desc`,
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

func TestOrderStateMatrixAllowsCompensationAndReturns(t *testing.T) {
	migration := readMigration(t, "order_db.sql")
	for _, forbidden := range []string{
		`status\s*<>\s*'cancelled'\s+or\s+fulfillment_status\s*=\s*'unfulfilled'`,
		`payment_status\s+not\s+in\s*\(\s*'partially_refunded'\s*,\s*'refunded'\s*\)\s+or\s+status\s*=\s*'cancelled'`,
	} {
		matched, err := regexp.MatchString(`(?s)`+forbidden, migration)
		if err != nil {
			t.Fatal(err)
		}
		if matched {
			t.Fatalf("migration retains over-restrictive state implication %q", forbidden)
		}
	}
}

func TestSeedPositionalInsertContractsRemainCompatible(t *testing.T) {
	contracts := map[string]map[string][]string{
		"catalog_db.sql": {
			"category": {"category_id", "name", "parent_id", "path"},
			"product":  {"product_id", "title", "description", "brand", "category_id", "status", "created_at"},
			"variant":  {"sku", "product_id", "title", "attributes", "barcode", "price_minor", "compare_at_minor", "currency", "weight_grams"},
		},
		"customer_db.sql": {
			"customer": {"customer_id", "email", "name", "phone", "status", "created_at"},
			"address":  {"address_id", "customer_id", "label", "recipient", "phone", "country_code", "province", "city", "district", "line1", "postal_code", "is_default"},
		},
		"inventory_db.sql": {
			"location":        {"location_id", "name", "type", "priority"},
			"inventory_level": {"sku", "location_id", "on_hand", "reserved", "updated_at", "available"},
			"reservation":     {"reservation_id", "order_id", "sku", "location_id", "quantity", "status", "expires_at", "idempotency_key"},
		},
		"order_db.sql": {
			"sales_order":          {"order_id", "order_no", "customer_id", "status", "payment_status", "fulfillment_status", "currency", "subtotal_minor", "discount_minor", "shipping_minor", "tax_minor", "total_minor", "shipping_address", "idempotency_key", "placed_at"},
			"order_item":           {"order_item_id", "order_id", "sku", "title", "quantity", "unit_price_minor", "discount_minor", "total_minor"},
			"order_status_history": {"event_id", "order_id", "from_status", "to_status", "reason", "occurred_at"},
		},
		"payment_db.sql": {
			"payment_intent":  {"payment_intent_id", "order_id", "amount_minor", "currency", "status", "provider", "provider_reference", "idempotency_key", "created_at"},
			"payment_attempt": {"attempt_id", "payment_intent_id", "status", "failure_code", "amount_minor", "created_at"},
			"refund":          {"refund_id", "payment_intent_id", "amount_minor", "status", "reason", "idempotency_key", "created_at"},
			"webhook_inbox":   {"provider_event_id", "event_type", "payload", "received_at", "processed_at"},
		},
		"fulfillment_db.sql": {
			"fulfillment_order": {"fulfillment_id", "order_id", "location_id", "status", "created_at"},
			"fulfillment_item":  {"fulfillment_id", "order_item_id", "sku", "quantity"},
			"shipment":          {"shipment_id", "fulfillment_id", "carrier", "tracking_number", "status", "shipped_at", "delivered_at"},
			"tracking_event":    {"tracking_event_id", "shipment_id", "status", "description", "location", "occurred_at"},
		},
	}

	for filename, tables := range contracts {
		migration := readMigration(t, filename)
		for table, want := range tables {
			t.Run(filename+"/"+table, func(t *testing.T) {
				got := tableColumns(t, migration, table)
				if strings.Join(got, ",") != strings.Join(want, ",") {
					t.Fatalf("columns=%v, want %v", got, want)
				}
			})
		}
	}
}

func TestDeferredCrossRowInvariantsNameTheirEnforcementTasks(t *testing.T) {
	requirements := map[string][]string{
		"catalog_db.sql":   {`category depth[\s\S]*?task 8`},
		"customer_db.sql":  {`default.address existence[\s\S]*?task 5`},
		"inventory_db.sql": {`movement.level reconciliation[\s\S]*?task 5[\s\S]*?task 8`},
		"order_db.sql":     {`allocation.total reconciliation[\s\S]*?task 6[\s\S]*?task 8`},
	}
	for filename, patterns := range requirements {
		migration := readMigration(t, filename)
		for _, pattern := range patterns {
			requirePattern(t, migration, pattern)
		}
	}
}

func tableColumns(t *testing.T, migration, table string) []string {
	t.Helper()
	prefix := "create table if not exists " + table
	start := strings.Index(migration, prefix)
	if start < 0 {
		t.Fatalf("table %s not found", table)
	}
	open := strings.Index(migration[start+len(prefix):], "(")
	if open < 0 {
		t.Fatalf("table %s opening parenthesis not found", table)
	}
	open += start + len(prefix)
	depth := 0
	closeAt := -1
	for index := open; index < len(migration); index++ {
		switch migration[index] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				closeAt = index
			}
		}
		if closeAt >= 0 {
			break
		}
	}
	if closeAt < 0 {
		t.Fatalf("table %s closing parenthesis not found", table)
	}

	var definitions []string
	partStart := open + 1
	depth = 0
	for index := partStart; index <= closeAt; index++ {
		if index < closeAt {
			switch migration[index] {
			case '(':
				depth++
			case ')':
				depth--
			}
		}
		if index == closeAt || (migration[index] == ',' && depth == 0) {
			definitions = append(definitions, strings.TrimSpace(migration[partStart:index]))
			partStart = index + 1
		}
	}

	var columns []string
	for _, definition := range definitions {
		fields := strings.Fields(definition)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "check", "primary", "unique", "foreign", "constraint":
			continue
		default:
			columns = append(columns, strings.Trim(fields[0], `"`))
		}
	}
	return columns
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
