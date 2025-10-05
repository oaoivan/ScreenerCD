package assets

import (
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// TokenType describes logical category of an asset.
type TokenType string

const (
	TokenTypeStable TokenType = "stable"
	TokenTypeNative TokenType = "native"
)

// TokenInfo represents normalized token metadata used by connectors and pricers.
type TokenInfo struct {
	Symbol   string
	Address  common.Address
	Decimals uint8
	Type     TokenType
	ChainID  uint64
	Wrapped  bool
}

// IsZero returns true if token lacks essential metadata.
func (t TokenInfo) IsZero() bool {
	return t.Address == (common.Address{}) || strings.TrimSpace(t.Symbol) == ""
}

// WithType returns a copy of token info tagged with the provided type.
func (t TokenInfo) WithType(typ TokenType) TokenInfo {
	t.Type = typ
	return t
}

// CanonicalSymbol uppercases symbol for stable comparison.
func (t TokenInfo) CanonicalSymbol() string {
	return strings.ToUpper(strings.TrimSpace(t.Symbol))
}
