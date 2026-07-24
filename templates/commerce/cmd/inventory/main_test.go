package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTask5MainsWireConfiguredHTTPTimeouts(t *testing.T) {
	for _, service := range []string{"catalog", "customer", "inventory"} {
		path := filepath.Join("..", service, "main.go")
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(source)
		for _, field := range []string{
			"ReadHeaderTimeout: settings.HTTP.ReadHeaderTimeout",
			"ReadTimeout:       settings.HTTP.ReadTimeout",
			"WriteTimeout:      settings.HTTP.WriteTimeout",
			"IdleTimeout:       settings.HTTP.IdleTimeout",
		} {
			if !strings.Contains(text, field) {
				t.Errorf("%s missing timeout mapping %q", service, field)
			}
		}
	}
}

func TestInventoryMainSupervisesAndWaitsForRelay(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, contract := range []string{
		"inventory.NewRelayLifecycle",
		"inventory.NewRuntimeReadiness",
		"relay.Done()",
		"relay.Wait(shutdownContext)",
		"defer publisher.Close()",
	} {
		if !strings.Contains(text, contract) {
			t.Errorf("inventory main missing lifecycle contract %q", contract)
		}
	}
}
