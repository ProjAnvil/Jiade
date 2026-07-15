// Package template 发现内嵌模板、解析清单、逐字渲染（解压 tar）。
package template

// Manifest template.yaml 清单（设计文档 §6）。随工程拷贝，被 list/up/seed 读。
type Manifest struct {
	Name        string     `yaml:"name"`
	Description string     `yaml:"description"`
	Version     string     `yaml:"version"`
	Databases   []Database `yaml:"databases"`
	Services    []Service  `yaml:"services"`
	Seed        Seed       `yaml:"seed"`
}

// Database 某业务库及其迁移 SQL。
type Database struct {
	Name    string `yaml:"name"`
	Migrate string `yaml:"migrate"`
}

// Service 某服务及其端口、所属库。
type Service struct {
	Name string `yaml:"name"`
	Port int    `yaml:"port"`
	DB   string `yaml:"db"`
}

// Seed fixture 生成器入口与规模。
type Seed struct {
	Entrypoint string   `yaml:"entrypoint"`
	Scales     []string `yaml:"scales"`
}
