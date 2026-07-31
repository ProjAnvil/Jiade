package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
)

func TestNewDisabledProviderDoesNotExport(t *testing.T) {
	provider, err := New(context.Background(), Config{Service: "ledger", Enabled: false})
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
	provider, err := New(context.Background(), Config{
		Service:  "ledger",
		Instance: "ledger-1",
		Enabled:  false,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	attributes := provider.resource.Attributes()
	assertResourceAttribute(t, attributes, "service.name", "ledger")
	assertResourceAttribute(t, attributes, "service.instance.id", "ledger-1")
}

func TestNewInstallsBaggagePropagation(t *testing.T) {
	provider, err := New(context.Background(), Config{Service: "ledger", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	member, err := baggage.NewMember("tenant", "acme")
	if err != nil {
		t.Fatal(err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatal(err)
	}
	ctx := baggage.ContextWithBaggage(context.Background(), bag)
	headers := mapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, headers)
	if got, want := headers["baggage"], "tenant=acme"; got != want {
		t.Fatalf("baggage header=%q, want %q", got, want)
	}
}

type mapCarrier map[string]string

func (c mapCarrier) Get(key string) string { return c[key] }

func (c mapCarrier) Set(key, value string) { c[key] = value }

func (c mapCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for key := range c {
		keys = append(keys, key)
	}
	return keys
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
