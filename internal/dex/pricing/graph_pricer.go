package pricing

import (
	"math"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/yourusername/screner/internal/util"
)

// TokenInfo описывает минимальные сведения о токене, необходимые для построения графа.
type TokenInfo struct {
	Address  common.Address
	Symbol   string
	Decimals int
}

// Result содержит итоговую USD-котировку и метаданные маршрута.
type Result struct {
	Price     float64
	Route     []string
	Weight    float64
	UpdatedAt time.Time
}

// Pricer описывает интерфейс графового USD-агрегатора.
type Pricer interface {
	RegisterToken(TokenInfo)
	RegisterStable(TokenInfo)
	UpdatePair(TokenInfo, TokenInfo, float64, float64, time.Time)
	ResolveUSD(TokenInfo) (Result, bool)
}

// edge хранит цену перехода между токенами.
type edge struct {
	price     float64
	weight    float64
	updatedAt time.Time
}

// GraphPricer — реализация графового агрегатора для USD-деривации.
type GraphPricer struct {
	mu      sync.RWMutex
	edges   map[string]map[string]edge
	symbols map[string]string
	stables map[string]bool
	maxHops int
}

const (
	defaultMaxHops     = 3
	minWeightEpsilon   = 1e-9
	equalWeightEpsilon = 1e-6
)

// NewGraphPricer создаёт графовый агрегатор с предопределёнными стейблами.
func NewGraphPricer(stables []TokenInfo) *GraphPricer {
	gp := &GraphPricer{
		edges:   make(map[string]map[string]edge),
		symbols: make(map[string]string),
		stables: make(map[string]bool),
		maxHops: defaultMaxHops,
	}
	for _, s := range stables {
		gp.RegisterStable(s)
	}
	return gp
}

// RegisterToken сохраняет символ токена для отображения маршрутов.
func (g *GraphPricer) RegisterToken(token TokenInfo) {
	addr := normalizeAddr(token.Address)
	if addr == "" {
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(token.Symbol))
	if symbol == "" {
		symbol = addr
	}
	g.mu.Lock()
	g.symbols[addr] = symbol
	g.mu.Unlock()
}

// RegisterStable помечает токен как USD-якорь.
func (g *GraphPricer) RegisterStable(token TokenInfo) {
	addr := normalizeAddr(token.Address)
	if addr == "" {
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(token.Symbol))
	if symbol == "" {
		symbol = addr
	}
	g.mu.Lock()
	g.symbols[addr] = symbol
	g.stables[addr] = true
	g.mu.Unlock()
	util.Infof("pricing: stable anchor %s (%s)", symbol, addr)
}

// UpdatePair обновляет цену перехода между токенами (a -> b) и обратное направление.
func (g *GraphPricer) UpdatePair(a TokenInfo, b TokenInfo, priceAB float64, weight float64, ts time.Time) {
	if priceAB <= 0 || math.IsInf(priceAB, 0) || math.IsNaN(priceAB) {
		return
	}
	if weight <= 0 || math.IsNaN(weight) || math.IsInf(weight, 0) {
		weight = minWeightEpsilon
	}
	if ts.IsZero() {
		ts = time.Now()
	}

	addrA := normalizeAddr(a.Address)
	addrB := normalizeAddr(b.Address)
	if addrA == "" || addrB == "" {
		return
	}

	g.RegisterToken(a)
	g.RegisterToken(b)

	g.mu.Lock()
	defer g.mu.Unlock()

	g.setEdgeLocked(addrA, addrB, priceAB, weight, ts)
	inv := 1.0 / priceAB
	if inv > 0 && !math.IsInf(inv, 0) && !math.IsNaN(inv) {
		g.setEdgeLocked(addrB, addrA, inv, weight, ts)
	}
	util.Debugf("pricing: update %s -> %s price=%.10f weight=%.4f", g.symbolUnsafe(addrA), g.symbolUnsafe(addrB), priceAB, weight)
}

// ResolveUSD ищет кратчайший маршрут до стейблкоина и возвращает USD-котировку.
func (g *GraphPricer) ResolveUSD(token TokenInfo) (Result, bool) {
	addr := normalizeAddr(token.Address)
	if addr == "" {
		return Result{}, false
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	if g.stables[addr] {
		return Result{
			Price:     1.0,
			Route:     []string{g.symbolUnsafe(addr)},
			Weight:    math.MaxFloat64,
			UpdatedAt: time.Now(),
		}, true
	}

	type queueItem struct {
		addr      string
		price     float64
		weight    float64
		freshness time.Time
		path      []string
		hops      int
	}

	visited := make(map[string]visitInfo)
	queue := []queueItem{{
		addr:   addr,
		price:  1.0,
		weight: math.MaxFloat64,
		path:   []string{addr},
		hops:   0,
	}}
	visited[addr] = visitInfo{weight: math.MaxFloat64, hops: 0}

	best := Result{Weight: -1}
	found := false

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		edges := g.edges[cur.addr]
		if len(edges) == 0 {
			continue
		}

		for nextAddr, e := range edges {
			if e.price <= 0 || math.IsNaN(e.price) || math.IsInf(e.price, 0) {
				continue
			}
			nextPrice := cur.price * e.price
			if nextPrice <= 0 || math.IsNaN(nextPrice) || math.IsInf(nextPrice, 0) {
				continue
			}
			nextWeight := cur.weight
			if e.weight < nextWeight {
				nextWeight = e.weight
			}
			if nextWeight <= 0 {
				continue
			}
			nextFresh := e.updatedAt
			if !cur.freshness.IsZero() && cur.freshness.Before(nextFresh) {
				nextFresh = cur.freshness
			}
			path := append(append([]string(nil), cur.path...), nextAddr)
			hops := cur.hops + 1

			if g.stables[nextAddr] {
				if !found || betterCandidate(best, Result{Price: nextPrice, Weight: nextWeight, UpdatedAt: nextFresh}, equalWeightEpsilon) {
					best = Result{
						Price:     nextPrice,
						Route:     g.symbolPath(path),
						Weight:    nextWeight,
						UpdatedAt: nextFresh,
					}
					found = true
				}
				continue
			}

			if hops >= g.maxHops {
				continue
			}

			if !shouldVisit(visited[nextAddr], nextWeight, hops) {
				continue
			}
			visited[nextAddr] = visitInfo{weight: nextWeight, hops: hops}
			queue = append(queue, queueItem{
				addr:      nextAddr,
				price:     nextPrice,
				weight:    nextWeight,
				freshness: nextFresh,
				path:      path,
				hops:      hops,
			})
		}
	}

	return best, found
}

type visitInfo struct {
	weight float64
	hops   int
}

func shouldVisit(prev visitInfo, weight float64, hops int) bool {
	if weight <= 0 {
		return false
	}
	if prev.weight == 0 && prev.hops == 0 {
		return true
	}
	if weight > prev.weight+equalWeightEpsilon {
		return true
	}
	if math.Abs(weight-prev.weight) <= equalWeightEpsilon && hops < prev.hops {
		return true
	}
	return false
}

func betterCandidate(current Result, candidate Result, eps float64) bool {
	if candidate.Weight > current.Weight+eps {
		return true
	}
	if math.Abs(candidate.Weight-current.Weight) <= eps && candidate.UpdatedAt.After(current.UpdatedAt) {
		return true
	}
	return false
}

func (g *GraphPricer) setEdgeLocked(from, to string, price float64, weight float64, ts time.Time) {
	neighbors, ok := g.edges[from]
	if !ok {
		neighbors = make(map[string]edge)
		g.edges[from] = neighbors
	}
	cur, ok := neighbors[to]
	if ok {
		if !ts.After(cur.updatedAt) && weight <= cur.weight+equalWeightEpsilon {
			return
		}
	}
	neighbors[to] = edge{price: price, weight: weight, updatedAt: ts}
}

func (g *GraphPricer) symbolUnsafe(addr string) string {
	if sym, ok := g.symbols[addr]; ok {
		return sym
	}
	return addr
}

func (g *GraphPricer) symbolPath(path []string) []string {
	out := make([]string, len(path))
	for i, addr := range path {
		out[i] = g.symbolUnsafe(addr)
	}
	return out
}

func normalizeAddr(addr common.Address) string {
	if (addr == common.Address{}) {
		return ""
	}
	return strings.ToLower(addr.Hex())
}
