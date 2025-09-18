package protobuf

// Minimal stub of generated protobuf struct used by the project.
// This file is intentionally small and only used to allow local builds
// until proper .proto -> .pb.go generation is performed.

type MarketData struct {
    Exchange  string  `json:"exchange"`
    Symbol    string  `json:"symbol"`
    Price     float64 `json:"price"`
    Timestamp int64   `json:"timestamp"`
}
