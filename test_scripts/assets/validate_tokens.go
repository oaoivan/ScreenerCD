package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"gopkg.in/yaml.v2"

	"github.com/yourusername/screner/internal/assets"
)

type rawToken struct {
	Symbol   string `yaml:"symbol"`
	Address  string `yaml:"address"`
	Decimals int    `yaml:"decimals"`
}

type rawNetwork struct {
	ChainID uint64     `yaml:"chain_id"`
	Stable  []rawToken `yaml:"stable"`
	Native  []rawToken `yaml:"native"`
}

func main() {
	tokensPath := flag.String("tokens", "configs/assets/tokens.yaml", "path to the tokens registry YAML")
	symbolLoaderPath := flag.String("symbol-loader", "internal/util/symbol_loader.go", "path to symbol_loader.go")
	symbolsFilePath := flag.String("symbols-file", "internal/util/symbols.go", "path to symbols.go")
	dryRun := flag.Bool("dry-run", false, "validate only without writing changes")
	flag.Parse()

	if err := execute(*tokensPath, *symbolLoaderPath, *symbolsFilePath, *dryRun); err != nil {
		log.Fatalf("validate tokens: %v", err)
	}
}

func execute(tokensPath, symbolLoaderPath, symbolsFilePath string, dryRun bool) error {
	absTokens, err := filepath.Abs(tokensPath)
	if err != nil {
		return fmt.Errorf("resolve tokens path: %w", err)
	}
	log.Printf("loading registry %s", absTokens)

	catalogs, err := assets.LoadRegistryFromFile(absTokens)
	if err != nil {
		return fmt.Errorf("load registry: %w", err)
	}

	rawData, err := loadRawRegistry(absTokens)
	if err != nil {
		return err
	}

	if err := validateRawRegistry(rawData); err != nil {
		return err
	}

	stableSet := make(map[string]struct{})
	for name, catalog := range catalogs {
		log.Printf("network=%s chain_id=%d stable=%d native=%d", name, catalog.Chain, len(catalog.Stable), len(catalog.Native))
		for _, token := range catalog.Stable {
			stableSet[token.CanonicalSymbol()] = struct{}{}
		}
	}

	stableList := uniqueSortedSymbols(stableSet)
	if len(stableList) == 0 {
		return fmt.Errorf("registry does not contain stable tokens")
	}

	absSymbolLoader, err := filepath.Abs(symbolLoaderPath)
	if err != nil {
		return fmt.Errorf("resolve symbol loader path: %w", err)
	}
	absSymbolsFile, err := filepath.Abs(symbolsFilePath)
	if err != nil {
		return fmt.Errorf("resolve symbols file path: %w", err)
	}

	missingSkip, rewroteSkip, err := ensureStableSkipSet(absSymbolLoader, stableList, dryRun)
	if err != nil {
		return err
	}
	missingSlice, rewroteSlice, err := ensureStableSlice(absSymbolsFile, stableList, dryRun)
	if err != nil {
		return err
	}

	if dryRun {
		if rewroteSkip || rewroteSlice {
			return fmt.Errorf("dry-run: skip-lists require updates (map added=%v slice added=%v)", missingSkip, missingSlice)
		}
		log.Printf("dry-run successful: no changes required")
		return nil
	}

	if rewroteSkip {
		if len(missingSkip) > 0 {
			log.Printf("stableSkipSet updated with symbols: %v", missingSkip)
		} else {
			log.Printf("stableSkipSet reordered to canonical order")
		}
	} else {
		log.Printf("stableSkipSet already up to date")
	}
	if rewroteSlice {
		if len(missingSlice) > 0 {
			log.Printf("isStablecoinOrFiat stablecoins updated with symbols: %v", missingSlice)
		} else {
			log.Printf("isStablecoinOrFiat stablecoins reordered to canonical order")
		}
	} else {
		log.Printf("isStablecoinOrFiat stablecoins already up to date")
	}

	log.Printf("validation completed successfully")
	return nil
}

func loadRawRegistry(path string) (map[string]rawNetwork, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	raw := make(map[string]rawNetwork)
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode registry yaml: %w", err)
	}
	return raw, nil
}

func validateRawRegistry(raw map[string]rawNetwork) error {
	if len(raw) == 0 {
		return fmt.Errorf("registry yaml is empty")
	}
	var warnings []string
	for name, network := range raw {
		if network.ChainID == 0 {
			return fmt.Errorf("network %s missing chain_id", name)
		}
		for _, token := range append(append([]rawToken(nil), network.Stable...), network.Native...) {
			if strings.TrimSpace(token.Symbol) == "" {
				return fmt.Errorf("network %s has token with empty symbol", name)
			}
			addr := strings.TrimSpace(token.Address)
			if !common.IsHexAddress(addr) {
				return fmt.Errorf("network %s token %s has invalid address %q", name, token.Symbol, token.Address)
			}
			canonical := common.HexToAddress(addr).Hex()
			if !strings.EqualFold(addr, canonical) {
				warnings = append(warnings, fmt.Sprintf("network %s token %s address not checksummed (have %s expect %s)", name, token.Symbol, addr, canonical))
			}
			if token.Decimals <= 0 || token.Decimals > 255 {
				return fmt.Errorf("network %s token %s has invalid decimals %d", name, token.Symbol, token.Decimals)
			}
		}
	}
	for _, w := range warnings {
		log.Printf("warning: %s", w)
	}
	return nil
}

func ensureStableSkipSet(path string, stables []string, dryRun bool) ([]string, bool, error) {
	fset := token.NewFileSet()
	fileAst, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}

	var lit *ast.CompositeLit
	for _, decl := range fileAst.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "stableSkipSet" {
				continue
			}
			if len(vs.Values) != 1 {
				return nil, false, fmt.Errorf("unexpected stableSkipSet initializer in %s", path)
			}
			var okLit bool
			lit, okLit = vs.Values[0].(*ast.CompositeLit)
			if !okLit {
				return nil, false, fmt.Errorf("stableSkipSet value is not composite literal in %s", path)
			}
		}
	}
	if lit == nil {
		return nil, false, fmt.Errorf("stableSkipSet not found in %s", path)
	}

	existing := make(map[string]struct{})
	currentOrder := make([]string, 0, len(lit.Elts))
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		keyLit, ok := kv.Key.(*ast.BasicLit)
		if !ok || keyLit.Kind != token.STRING {
			continue
		}
		key := strings.Trim(keyLit.Value, "\"")
		if key == "" {
			continue
		}
		existing[key] = struct{}{}
		currentOrder = append(currentOrder, key)
	}

	var missing []string
	for _, sym := range stables {
		if _, ok := existing[sym]; !ok {
			missing = append(missing, sym)
		}
	}
	sort.Strings(missing)

	union := make(map[string]struct{}, len(existing)+len(missing))
	for key := range existing {
		union[key] = struct{}{}
	}
	for _, sym := range missing {
		union[sym] = struct{}{}
	}

	keys := make([]string, 0, len(union))
	for key := range union {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	needsRewrite := len(missing) > 0 || !sameStringSlice(currentOrder, keys)
	if !needsRewrite {
		return nil, false, nil
	}
	if dryRun {
		if len(missing) > 0 {
			log.Printf("stableSkipSet missing symbols: %v", missing)
		}
		if len(missing) == 0 && !sameStringSlice(currentOrder, keys) {
			log.Printf("stableSkipSet requires reordering to match sorted list")
		}
		return missing, true, nil
	}

	newElts := make([]ast.Expr, 0, len(keys))
	for _, key := range keys {
		newElts = append(newElts, makeMapEntry(key))
	}
	lit.Elts = newElts

	var buf bytes.Buffer
	if err := format.Node(&buf, fset, fileAst); err != nil {
		return nil, false, fmt.Errorf("format %s: %w", path, err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		return nil, false, fmt.Errorf("write %s: %w", path, err)
	}
	return missing, true, nil
}

func ensureStableSlice(path string, stables []string, dryRun bool) ([]string, bool, error) {
	fset := token.NewFileSet()
	fileAst, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}

	var target *ast.CompositeLit
	ast.Inspect(fileAst, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		ident, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || ident.Name != "stablecoins" {
			return true
		}
		lit, ok := assign.Rhs[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		if _, ok := lit.Type.(*ast.ArrayType); !ok {
			return true
		}
		target = lit
		return false
	})

	if target == nil {
		return nil, false, fmt.Errorf("stablecoins literal not found in %s", path)
	}

	existing := make(map[string]struct{})
	currentOrder := make([]string, 0, len(target.Elts))
	for _, elt := range target.Elts {
		lit, ok := elt.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		key := strings.Trim(lit.Value, "\"")
		if key == "" {
			continue
		}
		existing[key] = struct{}{}
		currentOrder = append(currentOrder, key)
	}

	var missing []string
	for _, sym := range stables {
		if _, ok := existing[sym]; !ok {
			missing = append(missing, sym)
		}
	}
	sort.Strings(missing)

	union := make(map[string]struct{}, len(existing)+len(missing))
	for key := range existing {
		union[key] = struct{}{}
	}
	for _, sym := range missing {
		union[sym] = struct{}{}
	}

	keys := make([]string, 0, len(union))
	for key := range union {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	needsRewrite := len(missing) > 0 || !sameStringSlice(currentOrder, keys)
	if !needsRewrite {
		return nil, false, nil
	}
	if dryRun {
		if len(missing) > 0 {
			log.Printf("stablecoins slice missing symbols: %v", missing)
		}
		if len(missing) == 0 && !sameStringSlice(currentOrder, keys) {
			log.Printf("stablecoins slice requires reordering to match sorted list")
		}
		return missing, true, nil
	}

	newElts := make([]ast.Expr, 0, len(keys))
	for _, key := range keys {
		newElts = append(newElts, makeStringLit(key))
	}
	target.Elts = newElts

	var buf bytes.Buffer
	if err := format.Node(&buf, fset, fileAst); err != nil {
		return nil, false, fmt.Errorf("format %s: %w", path, err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		return nil, false, fmt.Errorf("write %s: %w", path, err)
	}
	return missing, true, nil
}

func makeMapEntry(sym string) *ast.KeyValueExpr {
	return &ast.KeyValueExpr{
		Key:   &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(sym)},
		Value: &ast.CompositeLit{},
	}
}

func makeStringLit(sym string) *ast.BasicLit {
	return &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(sym)}
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func uniqueSortedSymbols(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for sym := range set {
		out = append(out, sym)
	}
	sort.Strings(out)
	return out
}
