package template

import "testing"

func TestNew_DiscoversBank(t *testing.T) {
	r := mustRegistry(t)
	names, err := r.Names()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range names {
		if n == "bank" {
			found = true
		}
	}
	if !found {
		t.Errorf("bank is missing from the template list: %v", names)
	}
}

func TestManifest_Bank(t *testing.T) {
	r := mustRegistry(t)
	m, err := r.Manifest("bank")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "bank" {
		t.Errorf("name=%q want bank", m.Name)
	}
	// 7 services; every service listens on container port 8080 behind the
	// Traefik gateway (host-published on :18000). Per-service host ports were
	// removed when the topology gained a dedicated gateway.
	if len(m.Services) != 7 {
		t.Fatalf("services=%+v want 7", m.Services)
	}
	wantSvc := map[string]int{"core-banking": 8080, "customer": 8080, "payment": 8080, "reward": 8080, "risk": 8080, "loan": 8080, "wealth": 8080}
	for _, s := range m.Services {
		if port, ok := wantSvc[s.Name]; !ok || s.Port != port {
			t.Errorf("service %+v not in %v", s, wantSvc)
		}
	}
	// Spec B-4b: 7 libraries (+loan_db/wealth_db).
	if len(m.Databases) != 7 {
		t.Fatalf("databases=%+v want 7", m.Databases)
	}
	wantDB := map[string]bool{"core_db": true, "cust_db": true, "pay_db": true, "reward_db": true, "risk_db": true, "loan_db": true, "wealth_db": true}
	for _, d := range m.Databases {
		if !wantDB[d.Name] {
			t.Errorf("database %q not in %v", d.Name, wantDB)
		}
	}
}

func mustRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return r
}
