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
	// Spec B-1：3 服务（core-banking:8080 / customer:8081 / payment:8082）。
	if len(m.Services) != 3 {
		t.Fatalf("services=%+v want 3", m.Services)
	}
	wantSvc := map[string]int{"core-banking": 8080, "customer": 8081, "payment": 8082}
	for _, s := range m.Services {
		if port, ok := wantSvc[s.Name]; !ok || s.Port != port {
			t.Errorf("service %+v not in %v", s, wantSvc)
		}
	}
	// Spec B-1：3 库（core_db / cust_db / pay_db）。
	if len(m.Databases) != 3 {
		t.Fatalf("databases=%+v want 3", m.Databases)
	}
	wantDB := map[string]bool{"core_db": true, "cust_db": true, "pay_db": true}
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
