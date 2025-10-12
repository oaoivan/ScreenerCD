package config

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"

	"gopkg.in/yaml.v2"
)

type RedisConfig struct {
	Address  string `yaml:"address"`  // optional full address like "host:port"
	Host     string `yaml:"host"`     // optional host
	Port     int    `yaml:"port"`     // optional port
	Password string `yaml:"password"` // optional password
}

type Config struct {
	Exchange  string   `yaml:"exchange"`  // legacy single exchange
	Exchanges []string `yaml:"exchanges"` // preferred list of exchanges, e.g. ["bybit","gate"]
	Symbol    string   `yaml:"symbol"`
	APIKey    string   `yaml:"api_key"`
	Secret    string   `yaml:"secret"`
	// DefaultSymbolsFile allows pointing to a shared JSON with tickers
	DefaultSymbolsFile string `yaml:"default_symbols_file"`
	// SharedPools describes global pools/tickers source reused by all connectors
	SharedPools    PoolsSource `yaml:"shared_pools"`
	AssetsRegistry FileSource  `yaml:"assets_registry"`
	// ExchangeConfigs describe per-exchange symbol sources
	ExchangeConfigs []ExchangeConfig `yaml:"exchange_configs"`
	Redis           RedisConfig      `yaml:"redis"`

	// Performance and runtime tuning
	DataChannelBuffer     int `yaml:"data_channel_buffer"`      // default 8192
	RedisWorkers          int `yaml:"redis_workers"`            // default 8
	RedisPipelineSize     int `yaml:"redis_pipeline_size"`      // default 300
	SubscribeBatchSize    int `yaml:"subscribe_batch_size"`     // default 100
	SubscribeBatchPauseMs int `yaml:"subscribe_batch_pause_ms"` // default 150
	MetricsPeriodSec      int `yaml:"metrics_period_sec"`       // default 5

	// Bitget-specific rate limits
	BitgetSubscribeBatchSize int `yaml:"bitget_subscribe_batch_size"` // default 30
	BitgetSubscribePauseMs   int `yaml:"bitget_subscribe_pause_ms"`   // default 700
	BitgetPingIntervalSec    int `yaml:"bitget_ping_interval_sec"`    // default 25

	// DEX connectors
	DexConfigs []DexConfig `yaml:"dex_configs"`
}

// DexConfig описывает настройки DEX коннектора для конкретной сети.
type DexConfig struct {
	Name            string          `yaml:"name"`
	Network         string          `yaml:"network"`
	WSURL           string          `yaml:"ws_url"`
	HTTPURL         string          `yaml:"http_url"`
	SubscribeBatch  int             `yaml:"subscribe_batch"`
	PingInterval    int             `yaml:"ping_interval"`
	PoolManager     string          `yaml:"pool_manager"`
	Pools           []DexPoolConfig `yaml:"pools"`
	PoolsFile       string          `yaml:"pools_file"`
	PoolsSource     PoolsSource     `yaml:"pools_source"`
	AssetsSource    FileSource      `yaml:"assets_registry"`
	AssetsPath      string          `yaml:"-"`
	MaxMetaWorkers  int             `yaml:"max_meta_workers"`
	SwapOnly        bool            `yaml:"swap_only"`
	LogAllEvents    bool            `yaml:"log_all_events"`
	StopOnAckError  bool            `yaml:"stop_on_ack_error"`
	WantedPairs     []string        `yaml:"wanted_pairs"`
	WantedPairsOnly bool            `yaml:"wanted_pairs_only"`
	Once            bool            `yaml:"once"`
}

const (
	minSubscribeBatch = 1
	maxSubscribeBatch = 500
	minPingInterval   = 5
	maxPingInterval   = 120
	minMetaWorkers    = 1
	maxMetaWorkers    = 64
)

// ResolvePoolsPath возвращает путь для конкретного DEX коннектора с учётом shared fallback.
func (d DexConfig) ResolvePoolsPath(shared string) string {
	if path := strings.TrimSpace(d.PoolsSource.Resolve()); path != "" {
		return path
	}
	if path := strings.TrimSpace(os.ExpandEnv(d.PoolsFile)); path != "" {
		return path
	}
	return strings.TrimSpace(shared)
}

func (d DexConfig) ResolveAssetsPath(shared string) string {
	if path := strings.TrimSpace(d.AssetsSource.Resolve()); path != "" {
		return path
	}
	return strings.TrimSpace(shared)
}

func (d *DexConfig) applyDefaults() {
	if d.SubscribeBatch <= 0 {
		d.SubscribeBatch = 150
	}
	if d.PingInterval <= 0 {
		d.PingInterval = 25
	}
	if strings.EqualFold(d.Name, "uniswap_v4") && d.MaxMetaWorkers <= 0 {
		d.MaxMetaWorkers = 4
	}
}

func (d DexConfig) Validate() error {
	name := strings.TrimSpace(d.Name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(d.Network) == "" {
		return fmt.Errorf("network is required")
	}
	if strings.TrimSpace(d.WSURL) == "" {
		return fmt.Errorf("ws_url is required")
	}
	if d.SubscribeBatch < minSubscribeBatch || d.SubscribeBatch > maxSubscribeBatch {
		return fmt.Errorf("subscribe_batch must be in range %d..%d", minSubscribeBatch, maxSubscribeBatch)
	}
	if d.PingInterval < minPingInterval || d.PingInterval > maxPingInterval {
		return fmt.Errorf("ping_interval must be in range %d..%d", minPingInterval, maxPingInterval)
	}
	if d.WantedPairsOnly && len(d.WantedPairs) == 0 {
		return fmt.Errorf("wanted_pairs must be provided when wanted_pairs_only=true")
	}

	if strings.EqualFold(name, "uniswap_v4") {
		if strings.TrimSpace(d.HTTPURL) == "" {
			return fmt.Errorf("http_url is required for uniswap_v4")
		}
		if strings.TrimSpace(d.PoolManager) == "" {
			return fmt.Errorf("pool_manager is required for uniswap_v4")
		}
		if d.MaxMetaWorkers < minMetaWorkers || d.MaxMetaWorkers > maxMetaWorkers {
			return fmt.Errorf("max_meta_workers must be in range %d..%d", minMetaWorkers, maxMetaWorkers)
		}
		poolsPath := strings.TrimSpace(d.PoolsSource.Resolve())
		if poolsPath == "" {
			poolsPath = strings.TrimSpace(os.ExpandEnv(d.PoolsFile))
		}
		if len(d.Pools) == 0 && poolsPath == "" {
			return fmt.Errorf("either pools, pools_file or pools_source must be provided for uniswap_v4")
		}
	}

	return nil
}

// DexPoolConfig описывает минимальную информацию по пулу.
type DexPoolConfig struct {
	Address        string `yaml:"address"`
	PairName       string `yaml:"pair_name"`
	Token0Symbol   string `yaml:"token0_symbol"`
	Token1Symbol   string `yaml:"token1_symbol"`
	Token0Address  string `yaml:"token0_address"`
	Token1Address  string `yaml:"token1_address"`
	Token0Decimals uint8  `yaml:"token0_decimals"`
	Token1Decimals uint8  `yaml:"token1_decimals"`
	BaseIsToken0   bool   `yaml:"base_is_token0"`
	CanonicalPair  string `yaml:"canonical_pair"`
}

type FileSource struct {
	File string `yaml:"file"`
	Env  string `yaml:"env"`
}

func (fs FileSource) Resolve() string {
	if env := strings.TrimSpace(fs.Env); env != "" {
		if val := strings.TrimSpace(os.Getenv(env)); val != "" {
			return val
		}
	}
	if fs.File == "" {
		return ""
	}
	return strings.TrimSpace(os.ExpandEnv(fs.File))
}

// PoolsSource задаёт параметры внешнего списка пулов, например GeckoTerminal JSON.
type PoolsSource struct {
	File          string   `yaml:"file"`
	Env           string   `yaml:"env"`
	GeckoDex      string   `yaml:"gecko_dex"`
	GeckoNetwork  string   `yaml:"gecko_network"`
	IncludeStable bool     `yaml:"include_stable"`
	WantedPairs   []string `yaml:"wanted_pairs"`
}

// Resolve возвращает итоговый путь к файлу пулов с учётом env и подстановок.
func (ps PoolsSource) Resolve() string {
	return FileSource{File: ps.File, Env: ps.Env}.Resolve()
}

func (c *Config) ResolveAssetsRegistryPath() string {
	return strings.TrimSpace(c.AssetsRegistry.Resolve())
}

// ResolveSharedPoolsPath возвращает путь к общему JSON со списком пулов/тикеров для всех коннекторов.
// Приоритет: shared_pools, default_symbols_file, затем первые указанные пути в dex_configs.
func (c *Config) ResolveSharedPoolsPath() string {
	if path := strings.TrimSpace(c.SharedPools.Resolve()); path != "" {
		return path
	}
	if path := strings.TrimSpace(os.ExpandEnv(c.DefaultSymbolsFile)); path != "" {
		return path
	}
	for _, dex := range c.DexConfigs {
		if path := strings.TrimSpace(dex.PoolsSource.Resolve()); path != "" {
			return path
		}
		if path := strings.TrimSpace(os.ExpandEnv(dex.PoolsFile)); path != "" {
			return path
		}
	}
	return ""
}

// ExchangeConfig describes how to load symbols for a specific exchange
type ExchangeConfig struct {
	Name        string   `yaml:"name"`
	Symbols     []string `yaml:"symbols"`
	SymbolsFile string   `yaml:"symbols_file"`
}

func (r *RedisConfig) RedisAddress() string {
	if r.Address != "" {
		return r.Address
	}
	if r.Host != "" && r.Port != 0 {
		return fmt.Sprintf("%s:%d", r.Host, r.Port)
	}
	// fallback
	return "127.0.0.1:6380"
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

	// Defaults for Bitget specific settings
	if config.BitgetSubscribeBatchSize <= 0 {
		config.BitgetSubscribeBatchSize = 30
	}
	if config.BitgetSubscribePauseMs <= 0 {
		config.BitgetSubscribePauseMs = 700
	}
	if config.BitgetPingIntervalSec <= 0 {
		config.BitgetPingIntervalSec = 25
	}

	sharedAssets := strings.TrimSpace(config.AssetsRegistry.Resolve())
	for i := range config.DexConfigs {
		config.DexConfigs[i].WSURL = strings.TrimSpace(os.ExpandEnv(config.DexConfigs[i].WSURL))
		config.DexConfigs[i].HTTPURL = strings.TrimSpace(os.ExpandEnv(config.DexConfigs[i].HTTPURL))
		config.DexConfigs[i].PoolManager = strings.TrimSpace(os.ExpandEnv(config.DexConfigs[i].PoolManager))
		config.DexConfigs[i].applyDefaults()
		config.DexConfigs[i].AssetsPath = config.DexConfigs[i].ResolveAssetsPath(sharedAssets)
	}

	if err := config.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
		return nil, err
	}

	return &config, nil
}

func (c *Config) Validate() error {
	globalAssetsPath := strings.TrimSpace(c.ResolveAssetsRegistryPath())
	requiresV4 := false
	for i := range c.DexConfigs {
		if err := c.DexConfigs[i].Validate(); err != nil {
			name := strings.TrimSpace(c.DexConfigs[i].Name)
			if name == "" {
				name = fmt.Sprintf("index %d", i)
			}
			return fmt.Errorf("dex config %s: %w", name, err)
		}

		name := strings.ToLower(strings.TrimSpace(c.DexConfigs[i].Name))
		if name == "uniswap_v4" {
			requiresV4 = true
			dexAssetsPath := strings.TrimSpace(c.DexConfigs[i].AssetsPath)
			if dexAssetsPath == "" && globalAssetsPath == "" {
				return fmt.Errorf("dex config %s: assets registry path required for uniswap_v4", c.DexConfigs[i].Name)
			}
		}
	}

	if requiresV4 && globalAssetsPath == "" {
		// Валидация уже проверяет per-dex путь, но дополнительная проверка
		// помогает обнаружить забытый глобальный реестр для графового прайсера.
		return fmt.Errorf("assets_registry must be configured when uniswap_v4 is enabled")
	}
	return nil
}
