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
	Exchange  string      `yaml:"exchange"`  // legacy single exchange
	Exchanges []string    `yaml:"exchanges"` // preferred list of exchanges, e.g. ["bybit","gate"]
	Symbol    string      `yaml:"symbol"`
	APIKey    string      `yaml:"api_key"`
	Secret    string      `yaml:"secret"`
	Redis     RedisConfig `yaml:"redis"`

	// Performance and runtime tuning
	DataChannelBuffer     int `yaml:"data_channel_buffer"`      // default 8192
	RedisWorkers          int `yaml:"redis_workers"`            // default 8
	RedisPipelineSize     int `yaml:"redis_pipeline_size"`      // default 300
	SubscribeBatchSize    int `yaml:"subscribe_batch_size"`     // default 100
	SubscribeBatchPauseMs int `yaml:"subscribe_batch_pause_ms"` // default 150
	MetricsPeriodSec      int `yaml:"metrics_period_sec"`       // default 5
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

	// Defaults for new fields
	if config.DataChannelBuffer <= 0 {
		config.DataChannelBuffer = 8192
	}
	if config.RedisWorkers <= 0 {
		config.RedisWorkers = 8
	}
	if config.RedisPipelineSize <= 0 {
		config.RedisPipelineSize = 300
	}
	if config.SubscribeBatchSize <= 0 {
		config.SubscribeBatchSize = 100
	}
	if config.SubscribeBatchPauseMs <= 0 {
		config.SubscribeBatchPauseMs = 150
	}
	if config.MetricsPeriodSec <= 0 {
		config.MetricsPeriodSec = 5
	}

	return &config, nil
}
