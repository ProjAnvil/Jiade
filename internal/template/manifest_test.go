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
		t.Errorf("bank 未在模板列表: %v", names)
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
	// Spec B-4b：7 服务（+loan:18085 / wealth:18086）。
	if len(m.Services) != 7 {
		t.Fatalf("services=%+v want 7", m.Services)
	}
	wantSvc := map[string]int{"core-banking": 18080, "customer": 18081, "payment": 18082, "reward": 18083, "risk": 18084, "loan": 18085, "wealth": 18086}
	for _, s := range m.Services {
		if port, ok := wantSvc[s.Name]; !ok || s.Port != port {
			t.Errorf("service %+v not in %v", s, wantSvc)
		}
	}
	// Spec B-4b：7 库（+loan_db / wealth_db）。
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
