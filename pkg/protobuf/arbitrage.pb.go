package protobuf

// Minimal stub of generated protobuf struct used by the project.
// This file is intentionally small and only used to allow local builds
// until proper .proto -> .pb.go generation is performed.

// MarketData описывает единицу рыночных данных, которую мы публикуем в Redis и grpc-потребителям.
type MarketData struct {
	Exchange   string  `json:"exchange"`
	Symbol     string  `json:"symbol"`
	Price      float64 `json:"price"`
	Timestamp  int64   `json:"timestamp"`
	Network    string  `json:"network"`
	ChainID    uint32  `json:"chain_id"`
	Dex        string  `json:"dex"`
	AMMVersion string  `json:"amm_version"`
}
