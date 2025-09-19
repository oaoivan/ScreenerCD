package config

import (
	"fmt"
	"io/ioutil"
	"log"

	"gopkg.in/yaml.v2"
)

type RedisConfig struct {
	Address  string `yaml:"address"`  // optional full address like "host:port"
	Host     string `yaml:"host"`     // optional host
	Port     int    `yaml:"port"`     // optional port
	Password string `yaml:"password"` // optional password
}

type Config struct {
	Exchange  string      `yaml:"exchange"`   // legacy single exchange
	Exchanges []string    `yaml:"exchanges"`  // preferred list of exchanges, e.g. ["bybit","gate"]
	Symbol    string      `yaml:"symbol"`
	APIKey    string      `yaml:"api_key"`
	Secret    string      `yaml:"secret"`
	Redis     RedisConfig `yaml:"redis"`
}

func (r *RedisConfig) RedisAddress() string {
	if r.Address != "" {
		return r.Address
	}
	if r.Host != "" && r.Port != 0 {
		return fmt.Sprintf("%s:%d", r.Host, r.Port)
	}
	// fallback
	return "localhost:6379"
}

func LoadConfig(filePath string) (*Config, error) {
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		log.Fatalf("error reading config file: %v", err)
		return nil, err
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		log.Fatalf("error unmarshalling config: %v", err)
		return nil, err
	}

	return &config, nil
}
