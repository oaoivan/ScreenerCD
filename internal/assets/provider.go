package assets

import (
	"sort"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

type Provider struct {
	mu            sync.RWMutex
	catalogs      map[string]NetworkCatalog
	byChainTokens map[uint64]map[TokenType][]TokenInfo
	byChainSymbol map[uint64]map[string]TokenInfo
	byChainAddr   map[uint64]map[string]TokenInfo
}

var (
	cacheMu       sync.RWMutex
	providerCache = make(map[string]*Provider)
)

// LoadOrGetProvider loads registry from path or returns cached provider instance.
func LoadOrGetProvider(path string) (*Provider, error) {
	cacheMu.RLock()
	if p, ok := providerCache[path]; ok {
		cacheMu.RUnlock()
		return p, nil
	}
	cacheMu.RUnlock()

	catalogs, err := LoadRegistryFromFile(path)
	if err != nil {
		return nil, err
	}
	provider := NewProvider(catalogs)

	cacheMu.Lock()
	providerCache[path] = provider
	cacheMu.Unlock()
	return provider, nil
}

// NewProvider indexes catalogs and prepares immutable lookups.
func NewProvider(catalogs map[string]NetworkCatalog) *Provider {
	p := &Provider{
		catalogs:      make(map[string]NetworkCatalog, len(catalogs)),
		byChainTokens: make(map[uint64]map[TokenType][]TokenInfo),
		byChainSymbol: make(map[uint64]map[string]TokenInfo),
		byChainAddr:   make(map[uint64]map[string]TokenInfo),
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for rawName, catalog := range catalogs {
		key := strings.TrimSpace(rawName)
		stableCopy := cloneTokens(catalog.Stable)
		nativeCopy := cloneTokens(catalog.Native)

		p.catalogs[key] = NetworkCatalog{
			Name:   key,
			Chain:  catalog.Chain,
			Stable: stableCopy,
			Native: nativeCopy,
		}
		p.indexTokensLocked(catalog.Chain, stableCopy, TokenTypeStable)
		p.indexTokensLocked(catalog.Chain, nativeCopy, TokenTypeNative)
	}

	return p
}

func (p *Provider) indexTokensLocked(chainID uint64, tokens []TokenInfo, typ TokenType) {
	if chainID == 0 || len(tokens) == 0 {
		return
	}
	if _, ok := p.byChainTokens[chainID]; !ok {
		p.byChainTokens[chainID] = make(map[TokenType][]TokenInfo)
		p.byChainSymbol[chainID] = make(map[string]TokenInfo)
		p.byChainAddr[chainID] = make(map[string]TokenInfo)
	}
	for _, token := range tokens {
		p.byChainTokens[chainID][typ] = append(p.byChainTokens[chainID][typ], token)
		p.byChainSymbol[chainID][token.CanonicalSymbol()] = token
		p.byChainAddr[chainID][strings.ToLower(token.Address.Hex())] = token
	}
}

// TokensByNetwork returns tokens for network name filtered by type.
func (p *Provider) TokensByNetwork(network string, typ TokenType) []TokenInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	catalog, ok := p.catalogs[strings.TrimSpace(network)]
	if !ok {
		return nil
	}
	switch typ {
	case TokenTypeStable:
		return cloneTokens(catalog.Stable)
	case TokenTypeNative:
		return cloneTokens(catalog.Native)
	default:
		return nil
	}
}

// TokensByChain returns tokens for chain filtered by type.
func (p *Provider) TokensByChain(chainID uint64, typ TokenType) []TokenInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	tokensByType, ok := p.byChainTokens[chainID]
	if !ok {
		return nil
	}
	return cloneTokens(tokensByType[typ])
}

// FindBySymbol resolves token by chain and symbol.
func (p *Provider) FindBySymbol(chainID uint64, symbol string) (TokenInfo, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	bySym, ok := p.byChainSymbol[chainID]
	if !ok {
		return TokenInfo{}, false
	}
	token, ok := bySym[strings.ToUpper(strings.TrimSpace(symbol))]
	return token, ok
}

// FindByAddress resolves token by chain and address.
func (p *Provider) FindByAddress(chainID uint64, addr common.Address) (TokenInfo, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	byAddr, ok := p.byChainAddr[chainID]
	if !ok {
		return TokenInfo{}, false
	}
	token, ok := byAddr[strings.ToLower(addr.Hex())]
	return token, ok
}
func (p *Provider) NetworkNames() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	names := make([]string, 0, len(p.catalogs))
	for name := range p.catalogs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func cloneTokens(tokens []TokenInfo) []TokenInfo {
	if len(tokens) == 0 {
		return nil
	}
	out := make([]TokenInfo, len(tokens))
	copy(out, tokens)
	return out
}
