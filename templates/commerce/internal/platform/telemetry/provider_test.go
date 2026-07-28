package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

func TestNewDisabledProviderDoesNotExport(t *testing.T) {
	provider, err := New(context.Background(), Config{Service: "catalog", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDisabledProviderShutsDown(t *testing.T) {
	provider := Disabled()
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNewSetsServiceResourceIdentity(t *testing.T) {
	resource := newResource(Config{
		Service:  "catalog",
		Instance: "catalog-1",
	})

	attributes := resource.Attributes()
	assertResourceAttribute(t, attributes, "service.name", "catalog")
	assertResourceAttribute(t, attributes, "service.instance.id", "catalog-1")
}

func assertResourceAttribute(t *testing.T, attributes []attribute.KeyValue, key, want string) {
	t.Helper()
	for _, attribute := range attributes {
		if string(attribute.Key) == key && attribute.Value.AsString() == want {
			return
		}
	}
	t.Fatalf("resource attribute %q=%q not found in %v", key, want, attributes)
}
