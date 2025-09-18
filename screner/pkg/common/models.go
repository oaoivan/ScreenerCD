package common

// MarketData represents the standardized structure for market data from exchanges.
type MarketData struct {
    Exchange string  `json:"exchange"`
    Symbol   string  `json:"symbol"`
    Price    float64 `json:"price"`
    Timestamp int64  `json:"timestamp"`
}

// ArbitrageOpportunity represents an arbitrage opportunity between exchanges.
type ArbitrageOpportunity struct {
    Pair       string  `json:"pair"`
    Profit     float64 `json:"profit"`
    Exchange1  string  `json:"exchange1"`
    Exchange2  string  `json:"exchange2"`
    Timestamp  int64   `json:"timestamp"`
}