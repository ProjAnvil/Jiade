package grpcx_test

import (
	"context"
	"testing"

	customerv1 "bank/gen/bank/customer/v1"
	corev1 "bank/gen/bank/core/v1"
	"google.golang.org/grpc"
)

type customerQueryClient interface {
	GetCustomer(context.Context, *customerv1.GetCustomerRequest, ...grpc.CallOption) (*customerv1.CustomerSnapshot, error)
}

type accountQueryClient interface {
	GetAccount(context.Context, *corev1.GetAccountRequest, ...grpc.CallOption) (*corev1.AccountSnapshot, error)
}

func TestQueryContractsExposeRequiredMethods(t *testing.T) {
	var customer customerv1.CustomerQueryServiceClient
	var account corev1.AccountQueryServiceClient

	var _ customerQueryClient = customer
	var _ accountQueryClient = account

	if customer == nil || account == nil {
		t.Log("compile-time contract check")
	}
}
