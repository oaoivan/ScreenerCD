package assets

import (
	"fmt"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"gopkg.in/yaml.v2"
)

// NetworkCatalog groups stable and native assets for a blockchain network.
type NetworkCatalog struct {
	Name   string
	Chain  uint64
	Stable []TokenInfo
	Native []TokenInfo
}

type fileToken struct {
	Symbol   string `yaml:"symbol"`
	Address  string `yaml:"address"`
	Decimals int    `yaml:"decimals"`
	Wrapped  bool   `yaml:"wrapped"`
}

type fileNetwork struct {
	ChainID uint64      `yaml:"chain_id"`
	Stable  []fileToken `yaml:"stable"`
	Native  []fileToken `yaml:"native"`
}

// LoadRegistryFromFile parses YAML registry and returns catalogs per network.
func LoadRegistryFromFile(path string) (map[string]NetworkCatalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("assets: read registry: %w", err)
	}
	return ParseRegistryYAML(data)
}

// ParseRegistryYAML parses YAML bytes into network catalogs (useful for tests).
func ParseRegistryYAML(data []byte) (map[string]NetworkCatalog, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("assets: empty registry payload")
	}
	raw := make(map[string]fileNetwork)
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("assets: decode registry yaml: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("assets: registry does not contain networks")
	}

	catalogs := make(map[string]NetworkCatalog, len(raw))
	for name, net := range raw {
		catalog, err := convertNetwork(name, net)
		if err != nil {
			return nil, err
		}
		catalogs[name] = catalog
	}
	return catalogs, nil
}

func convertNetwork(name string, raw fileNetwork) (NetworkCatalog, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return NetworkCatalog{}, fmt.Errorf("assets: network name is empty")
	}
	if raw.ChainID == 0 {
		return NetworkCatalog{}, fmt.Errorf("assets: network %s missing chain_id", name)
	}

	stable, err := convertTokens(raw.Stable, raw.ChainID, TokenTypeStable, name)
	if err != nil {
		return NetworkCatalog{}, err
	}
	native, err := convertTokens(raw.Native, raw.ChainID, TokenTypeNative, name)
	if err != nil {
		return NetworkCatalog{}, err
	}

	return NetworkCatalog{
		Name:   name,
		Chain:  raw.ChainID,
		Stable: stable,
		Native: native,
	}, nil
}

func convertTokens(tokens []fileToken, chainID uint64, typ TokenType, network string) ([]TokenInfo, error) {
	result := make([]TokenInfo, 0, len(tokens))
	seenSymbols := make(map[string]struct{}, len(tokens))
	seenAddresses := make(map[string]struct{}, len(tokens))

	for idx, t := range tokens {
		info, err := convertToken(t, chainID, typ, network, idx)
		if err != nil {
			return nil, err
		}
		symKey := info.CanonicalSymbol()
		if _, exists := seenSymbols[symKey]; exists {
			return nil, fmt.Errorf("assets: duplicate symbol %s in %s %s", info.Symbol, network, typ)
		}
		addrKey := strings.ToLower(info.Address.Hex())
		if _, exists := seenAddresses[addrKey]; exists {
			return nil, fmt.Errorf("assets: duplicate address %s in %s %s", info.Address.Hex(), network, typ)
		}
		seenSymbols[symKey] = struct{}{}
		seenAddresses[addrKey] = struct{}{}
		result = append(result, info)
	}
	return result, nil
}

func convertToken(tok fileToken, chainID uint64, typ TokenType, network string, idx int) (TokenInfo, error) {
	symbol := strings.ToUpper(strings.TrimSpace(tok.Symbol))
	if symbol == "" {
		return TokenInfo{}, fmt.Errorf("assets: token #%d in %s %s missing symbol", idx, network, typ)
	}

	addrStr := strings.TrimSpace(tok.Address)
	if !common.IsHexAddress(addrStr) {
		return TokenInfo{}, fmt.Errorf("assets: token %s in %s %s has invalid address %q", symbol, network, typ, tok.Address)
	}
	address := common.HexToAddress(addrStr)

	if tok.Decimals <= 0 || tok.Decimals > 255 {
		return TokenInfo{}, fmt.Errorf("assets: token %s in %s %s has invalid decimals %d", symbol, network, typ, tok.Decimals)
	}

	info := TokenInfo{
		Symbol:   symbol,
		Address:  address,
		Decimals: uint8(tok.Decimals),
		ChainID:  chainID,
		Wrapped:  tok.Wrapped,
	}.WithType(typ)

	if info.IsZero() {
		return TokenInfo{}, fmt.Errorf("assets: token %s in %s %s missing mandatory fields", symbol, network, typ)
	}

	return info, nil
}
