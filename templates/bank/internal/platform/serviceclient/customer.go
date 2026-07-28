package serviceclient

import (
	"context"

	customerv1 "bank/gen/bank/customer/v1"
)

// Customer is the customer data needed by bank service compositions.
type Customer struct {
	CustomerID   string
	Name         string
	CustomerType string
	KYCStatus    string
	Status       string
	RiskTags     []string
}

// CustomerReader reads a customer snapshot without exposing transport details.
type CustomerReader interface {
	GetCustomer(context.Context, string, string) (Customer, error)
}

type customerReader struct {
	client customerv1.CustomerQueryServiceClient
}

// NewCustomerReader adapts the generated customer query client to CustomerReader.
func NewCustomerReader(client customerv1.CustomerQueryServiceClient) CustomerReader {
	return customerReader{client: client}
}

func (r customerReader) GetCustomer(ctx context.Context, customerID, requestID string) (Customer, error) {
	snapshot, err := r.client.GetCustomer(ctx, &customerv1.GetCustomerRequest{CustomerId: customerID, RequestId: requestID})
	if err != nil {
		return Customer{}, mapNotFound(err)
	}
	return Customer{
		CustomerID:   snapshot.GetCustomerId(),
		Name:         snapshot.GetName(),
		CustomerType: snapshot.GetCustomerType(),
		KYCStatus:    snapshot.GetKycStatus(),
		Status:       snapshot.GetStatus(),
		RiskTags:     append([]string(nil), snapshot.GetRiskTags()...),
	}, nil
}
