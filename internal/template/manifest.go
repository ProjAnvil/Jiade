// Package template discovers embedded templates, parses manifests, and renders verbatim (unpacked tar).
package template

// Manifest template.yaml Manifest (Design Document §6). Copied with the project and read by list/up/seed.
type Manifest struct {
	Name        string     `yaml:"name"`
	Description string     `yaml:"description"`
	Version     string     `yaml:"version"`
	Databases   []Database `yaml:"databases"`
	Services    []Service  `yaml:"services"`
	Seed        Seed       `yaml:"seed"`
}

// Database A business database and its migration SQL.
type Database struct {
	Name    string `yaml:"name"`
	Migrate string `yaml:"migrate"`
}

// Service A service, its port, and its affiliated library.
type Service struct {
	Name string `yaml:"name"`
	Port int    `yaml:"port"`
	DB   string `yaml:"db"`
}

// Seed fixture generator entry and scale.
type Seed struct {
	Entrypoint string   `yaml:"entrypoint"`
	Scales     []string `yaml:"scales"`
}
