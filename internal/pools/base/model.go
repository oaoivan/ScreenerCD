package basepools

import "encoding/json"

// File describes the JSON payload stored in ticker_source/base_pools.json.
type File struct {
	GeneratedAt string   `json:"generated_at"`
	Sources     []string `json:"sources"`
	Entries     []Entry  `json:"entries"`
	Stats       *Stats   `json:"stats,omitempty"`
}

// Entry represents a single pool record from the dataset.
type Entry struct {
	Label        string          `json:"label,omitempty"`
	Symbol       string          `json:"symbol"`
	AMMVersion   string          `json:"amm_version"`
	Dex          string          `json:"dex"`
	Network      string          `json:"network"`
	TokenAddress string          `json:"token_address,omitempty"`
	PoolID       string          `json:"pool_id,omitempty"`
	PoolAddress  string          `json:"pool_address"`
	PoolManager  string          `json:"pool_manager,omitempty"`
	PairName     string          `json:"pair_name,omitempty"`
	BaseToken    string          `json:"base_token,omitempty"`
	QuoteToken   string          `json:"quote_token,omitempty"`
	Source       string          `json:"source,omitempty"`
	PoolCreated  string          `json:"pool_created_at,omitempty"`
	FeePercent   string          `json:"fee_percent,omitempty"`
	LiquidityUSD float64         `json:"liquidity_usd,omitempty"`
	Token0       Token           `json:"token0"`
	Token1       Token           `json:"token1"`
	PoolKey      PoolKey         `json:"pool_key"`
	Extras       json.RawMessage `json:"extras,omitempty"`
	Meta         json.RawMessage `json:"meta,omitempty"`
	Notes        json.RawMessage `json:"notes,omitempty"`
}

// Token describes basic token metadata within a pool record.
type Token struct {
	Address  string `json:"address"`
	Symbol   string `json:"symbol"`
	Decimals int    `json:"decimals"`
	Source   string `json:"source,omitempty"`
}

// PoolKey contains parameters required to subscribe to a pool.
type PoolKey struct {
	Currency0   string `json:"currency0"`
	Currency1   string `json:"currency1"`
	Fee         *int   `json:"fee,omitempty"`
	TickSpacing *int   `json:"tickSpacing,omitempty"`
	Hooks       string `json:"hooks,omitempty"`
	Status      string `json:"status,omitempty"`
}

// Stats carries aggregated counters included in the dataset.
type Stats struct {
	Total     int     `json:"total"`
	Kept      int     `json:"kept"`
	Threshold float64 `json:"threshold"`
}
