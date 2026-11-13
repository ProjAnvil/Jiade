package seed

import (
	"fmt"
	"math/rand"
	"time"
)

// generateCustomers populates membership tiers, customers, and at least one
// default address per customer (the customer service enforces default-address
// existence; we mirror that invariant here).
func generateCustomers(ds *Dataset, c counts, seed int64, generatedAt time.Time) {
	stream := domainStream(seed, "customers")

	// --- Membership tiers: always all three, rank ordered.
	ds.MembershipTiers = make([]MembershipTierRow, 0, len(membershipTiers))
	for _, tier := range membershipTiers {
		ds.MembershipTiers = append(ds.MembershipTiers, MembershipTierRow{
			TierID:            tier.ID,
			Name:              tier.Name,
			Rank:              tier.Rank,
			MinimumSpendMinor: tier.MinimumSpendMinor,
		})
	}

	// --- Customers: each customer is bound to one region. ~5% are guest
	// checkouts; ~3% are disabled (soft-deleted).
	ds.Customers = make([]CustomerRow, 0, c.Customers)
	ds.Addresses = make([]AddressRow, 0, c.Customers*2)
	for customerIndex := 0; customerIndex < c.Customers; customerIndex++ {
		region := customerRegions[customerIndex%len(customerRegions)]
		customerID := fmt.Sprintf("cust-%06d", customerIndex+1)

		var name, email, phone string
		if region.Country == "China" {
			surname := pick(stream, customerSurnamesCN)
			given := pick(stream, customerGivenNamesCN)
			name = surname + given
			email = fmt.Sprintf("user%d@example.cn", customerIndex+1)
			phone = fmt.Sprintf("8613%09d", stream.Int63n(1_000_000_000))
		} else if region.Country == "United States" {
			surname := pick(stream, customerSurnamesEN)
			given := pick(stream, customerGivenNamesEN)
			name = given + " " + surname
			email = fmt.Sprintf("user%d@example.com", customerIndex+1)
			phone = fmt.Sprintf("+1-415-%03d-%04d", stream.Intn(1000), stream.Intn(10000))
		} else {
			surname := pick(stream, customerSurnamesEN)
			given := pick(stream, customerGivenNamesEN)
			name = given + " " + surname
			email = fmt.Sprintf("user%d@example.co.uk", customerIndex+1)
			phone = fmt.Sprintf("+44-20-%04d-%04d", stream.Intn(10000), stream.Intn(10000))
		}

		status := "active"
		switch {
		case boolP(stream, 0.05):
			status = "guest"
		case boolP(stream, 0.03):
			status = "disabled"
		}

		var phonePtr *string
		if status != "guest" {
			phonePtr = &phone
		}
		ds.Customers = append(ds.Customers, CustomerRow{
			CustomerID: customerID,
			Email:      email,
			Name:       name,
			Phone:      phonePtr,
			Status:     status,
			CreatedAt:  generatedAt.Add(-time.Duration(intRange(stream, 1, 365)) * 24 * time.Hour),
		})

		// Default address: every customer has exactly one (matches the
		// customer-service default-address invariant).
		var street string
		if region.Country == "China" {
			street = pick(stream, streetNamesCN) + fmt.Sprintf(" %d号", intRange(stream, 1, 999))
		} else {
			street = fmt.Sprintf("%d %s", intRange(stream, 100, 9999), pick(stream, streetNamesEN))
		}
		postalCode := postalFor(region.Code, stream)
		ds.Addresses = append(ds.Addresses, AddressRow{
			AddressID:   fmt.Sprintf("addr-%06d-1", customerIndex+1),
			CustomerID:  customerID,
			Label:       "Home",
			Recipient:   name,
			Phone:       phone,
			CountryCode: region.Code,
			Province:    region.Region,
			City:        region.City,
			District:    region.District,
			Line1:       street,
			PostalCode:  postalCode,
			IsDefault:   true,
		})

		// ~20% of customers have a second non-default address (work).
		if boolP(stream, 0.20) {
			altStreet := street
			if region.Country == "China" {
				altStreet = pick(stream, streetNamesCN) + fmt.Sprintf(" %d号", intRange(stream, 1, 999))
			} else {
				altStreet = fmt.Sprintf("%d %s", intRange(stream, 100, 9999), pick(stream, streetNamesEN))
			}
			ds.Addresses = append(ds.Addresses, AddressRow{
				AddressID:   fmt.Sprintf("addr-%06d-2", customerIndex+1),
				CustomerID:  customerID,
				Label:       "Work",
				Recipient:   name,
				Phone:       phone,
				CountryCode: region.Code,
				Province:    region.Region,
				City:        region.City,
				District:    region.District,
				Line1:       altStreet,
				PostalCode:  postalCode,
				IsDefault:   false,
			})
		}
	}
}

// postalFor returns a deterministic postal code shape per country. We do not
// need to be RFC-valid; we only need a non-empty string.
func postalFor(countryCode string, stream *rand.Rand) string {
	switch countryCode {
	case "CN":
		return fmt.Sprintf("2%04d", stream.Intn(100000))
	case "US":
		return fmt.Sprintf("%05d", stream.Intn(100000))
	case "GB":
		return fmt.Sprintf("SW1A %dAA", stream.Intn(10))
	default:
		return fmt.Sprintf("%05d", stream.Intn(100000))
	}
}
