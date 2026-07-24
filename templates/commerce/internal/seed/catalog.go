package seed

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"time"
)

// generateCatalog populates categories, brands, products, and variants. It is
// the first domain stream so any downstream generator can rely on a stable
// product catalogue.
func generateCatalog(ds *Dataset, c counts, seed int64, generatedAt time.Time) {
	stream := domainStream(seed, "catalog")

	// --- Categories: one root per slice index, plus an optional leaf. The dev
	// scale uses exactly 8 root categories (the spec mandates 8).
	categoryCount := c.Categories
	if categoryCount > len(catalogCategoryRoots) {
		categoryCount = len(catalogCategoryRoots)
	}
	ds.Categories = make([]CategoryRow, 0, categoryCount*2)
	for index := 0; index < categoryCount; index++ {
		root := catalogCategoryRoots[index]
		ds.Categories = append(ds.Categories, CategoryRow{
			CategoryID: root.ID,
			Name:       root.Name,
			ParentID:   nil,
			Path:       root.Path,
		})
		// Each root gets exactly one curated leaf so the tree depth stays 2.
		leaf := categoryLeaves[index%len(categoryLeaves)]
		leafID := root.ID + leaf.Suffix
		leafPath := root.Path + leaf.Suffix
		ds.Categories = append(ds.Categories, CategoryRow{
			CategoryID: leafID,
			Name:       root.Name + " " + leaf.Name,
			ParentID:   &root.ID,
			Path:       leafPath,
		})
	}

	// --- Brands: deterministic picks from the curated list.
	brandCount := c.Brands
	if brandCount > len(catalogBrands) {
		brandCount = len(catalogBrands)
	}
	ds.Brands = make([]BrandRow, 0, brandCount)
	chosenBrands := pickN(stream, len(catalogBrands), brandCount)
	for _, idx := range chosenBrands {
		name := catalogBrands[idx]
		ds.Brands = append(ds.Brands, BrandRow{
			BrandID:   fmt.Sprintf("brand-%03d", idx+1),
			Name:      name,
			Slug:      slugify(name),
			Status:    "active",
			CreatedAt: generatedAt.Add(-30 * 24 * time.Hour),
		})
	}

	// --- Products: 1 variant per product minimum (keeps dev summary stable);
	// some products get extra colour/size variants to exercise variant_option.
	ds.Products = make([]ProductRow, 0, c.Products)
	ds.Variants = make([]VariantRow, 0, c.Products*2)
	for productIndex := 0; productIndex < c.Products; productIndex++ {
		// Even spread across leaf categories so no leaf is empty.
		leafCategories := leafCategoryIDs(ds.Categories)
		leaf := leafCategories[productIndex%len(leafCategories)]
		brand := ds.Brands[productIndex%len(ds.Brands)]

		adjective := pick(stream, productAdjectives)
		noun := pick(stream, productNouns)
		title := fmt.Sprintf("%s %s %s", adjective, brand.Name, noun)
		productID := fmt.Sprintf("prod-%05d", productIndex+1)
		ds.Products = append(ds.Products, ProductRow{
			ProductID:   productID,
			Title:       title,
			Description: fmt.Sprintf("%s. Curated for the %s collection.", title, leaf),
			Brand:       brand.Name,
			CategoryID:  leaf,
			Status:      productStatus(stream, productIndex),
			CreatedAt:   generatedAt.Add(-time.Duration(intRange(stream, 1, 60)) * 24 * time.Hour),
		})

		// Variant count: 1 base variant, plus 0-2 extras to keep the dev
		// variant total deterministic but non-trivial.
		variantCount := 1
		if boolP(stream, 0.30) {
			variantCount++
		}
		if boolP(stream, 0.10) {
			variantCount++
		}
		basePrice := minorRange(stream, 1990, 299900) // 19.90 .. 2999.00
		for variantIndex := 0; variantIndex < variantCount; variantIndex++ {
			color := pick(stream, productColors)
			size := pick(stream, productSizes)
			attrs := map[string]string{
				"color": color,
				"size":  size,
			}
			if variantIndex > 0 {
				attrs["variant"] = fmt.Sprintf("v%d", variantIndex+1)
			}
			attributesJSON, _ := json.Marshal(attrs)
			price := basePrice + int64(variantIndex)*minorRange(stream, 100, 1500)
			var compareAt *int64
			if boolP(stream, 0.25) {
				markup := minorRange(stream, 500, 5000)
				value := price + markup
				compareAt = &value
			}
			barcode := fmt.Sprintf("0%012d", productIndex*100+variantIndex+1)
			sku := fmt.Sprintf("sku-%05d-%d", productIndex+1, variantIndex+1)
			ds.Variants = append(ds.Variants, VariantRow{
				SKU:            sku,
				ProductID:      productID,
				Title:          fmt.Sprintf("%s / %s / %s", title, color, size),
				Attributes:     attributesJSON,
				Barcode:        &barcode,
				PriceMinor:     price,
				CompareAtMinor: compareAt,
				Currency:       "USD",
				WeightGrams:    intRange(stream, 80, 4500),
			})
		}
	}
}

func productStatus(stream *rand.Rand, index int) string {
	roll := stream.Float64()
	switch {
	case roll < 0.10:
		return "archived"
	case roll < 0.25:
		return "draft"
	default:
		return "active"
	}
}

func leafCategoryIDs(categories []CategoryRow) []string {
	var leaves []string
	for _, c := range categories {
		if c.ParentID != nil {
			leaves = append(leaves, c.CategoryID)
		}
	}
	if len(leaves) == 0 {
		// Defensive: if the generator ever produces no leaves, fall back to
		// the root IDs so callers never panic.
		for _, c := range categories {
			leaves = append(leaves, c.CategoryID)
		}
	}
	return leaves
}

func slugify(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		case r == ' ' || r == '-':
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "brand"
	}
	return string(out)
}
