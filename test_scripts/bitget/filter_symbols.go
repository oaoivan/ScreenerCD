package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yourusername/screner/internal/util"
)

// Offline utility: fetch Bitget SPOT symbols via REST, intersect with base
// symbols from Temp/all_contracts_merged_reformatted.json (USDT pairs),
// and save to Temp/bitget_usdt_intersection.txt.
//
// Logging is verbose to make the process transparent.

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	baseFile := "Temp/all_contracts_merged_reformatted.json"
	outFile := "Temp/bitget_usdt_intersection.txt"

	log.Printf("[INFO] loading base symbols from %s", baseFile)
	baseSymbols, err := util.LoadSymbolsFromFile(baseFile)
	if err != nil {
		log.Fatalf("[FATAL] load base symbols: %v", err)
	}
	log.Printf("[INFO] base symbols (USDT) loaded: %d", len(baseSymbols))

	baseSet := make(map[string]struct{}, len(baseSymbols))
	for _, s := range baseSymbols {
		baseSet[strings.ToUpper(s)] = struct{}{}
	}

	bitgetSymbols, err := fetchBitgetSpotUSDT()
	if err != nil {
		log.Fatalf("[FATAL] fetch Bitget symbols: %v", err)
	}
	log.Printf("[INFO] bitget spot USDT symbols: %d", len(bitgetSymbols))

	// Intersect: keep only those present in base
	var inter []string
	for _, s := range bitgetSymbols {
		if _, ok := baseSet[s]; ok {
			inter = append(inter, s)
		}
	}
	sort.Strings(inter)
	log.Printf("[INFO] intersection size: %d", len(inter))
	if len(inter) > 0 {
		maxShow := len(inter)
		if maxShow > 15 {
			maxShow = 15
		}
		log.Printf("[DBG ] sample: %v ...", inter[:maxShow])
	}

	// Write to file
	if err := writeLines(outFile, inter); err != nil {
		log.Fatalf("[FATAL] write output: %v", err)
	}
	log.Printf("[INFO] written %d symbols to %s", len(inter), outFile)
}

// fetchBitgetSpotUSDT collects USDT spot symbols from Bitget REST.
// It tries v2 endpoint first, then falls back to v1 if needed.
func fetchBitgetSpotUSDT() ([]string, error) {
	client := &http.Client{Timeout: 20 * time.Second}

	// Try v2
	v2URL := "https://api.bitget.com/api/v2/spot/public/symbols"
	log.Printf("[INFO] requesting Bitget v2 symbols: %s", v2URL)
	if syms, err := requestBitgetSymbolsV2(client, v2URL); err == nil && len(syms) > 0 {
		log.Printf("[INFO] v2 symbols received: %d", len(syms))
		return normalizeAndFilterUSDT(syms), nil
	} else {
		if err != nil {
			log.Printf("[WARN] v2 request failed: %v", err)
		} else {
			log.Printf("[WARN] v2 returned 0 symbols, falling back to v1")
		}
	}

	// Fallback to v1
	v1URL := "https://api.bitget.com/api/spot/v1/public/symbols"
	log.Printf("[INFO] requesting Bitget v1 symbols: %s", v1URL)
	syms, err := requestBitgetSymbolsV1(client, v1URL)
	if err != nil {
		return nil, fmt.Errorf("v1 request failed: %w", err)
	}
	log.Printf("[INFO] v1 symbols received: %d", len(syms))
	return normalizeAndFilterUSDT(syms), nil
}

func requestBitgetSymbolsV2(client *http.Client, url string) ([]string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(b))
	}
	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Symbol    string `json:"symbol"`
			InstId    string `json:"instId"`
			BaseCoin  string `json:"baseCoin"`
			QuoteCoin string `json:"quoteCoin"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Code != "00000" {
		return nil, fmt.Errorf("api code=%s msg=%s", out.Code, out.Msg)
	}
	set := make(map[string]struct{})
	for _, d := range out.Data {
		sym := strings.ToUpper(firstNonEmpty(d.Symbol, d.InstId))
		if sym == "" {
			// fallback compose
			if d.BaseCoin != "" && d.QuoteCoin != "" {
				sym = strings.ToUpper(d.BaseCoin + d.QuoteCoin)
			}
		}
		if sym == "" {
			continue
		}
		set[sym] = struct{}{}
	}
	return setToSlice(set), nil
}

func requestBitgetSymbolsV1(client *http.Client, url string) ([]string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(b))
	}
	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Symbol    string `json:"symbol"`
			BaseCoin  string `json:"baseCoin"`
			QuoteCoin string `json:"quoteCoin"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Code != "00000" {
		return nil, fmt.Errorf("api code=%s msg=%s", out.Code, out.Msg)
	}
	set := make(map[string]struct{})
	for _, d := range out.Data {
		sym := strings.ToUpper(d.Symbol)
		if sym == "" && d.BaseCoin != "" && d.QuoteCoin != "" {
			sym = strings.ToUpper(d.BaseCoin + d.QuoteCoin)
		}
		if strings.HasSuffix(sym, "_SPBL") {
			sym = strings.TrimSuffix(sym, "_SPBL")
		}
		if sym == "" {
			continue
		}
		set[sym] = struct{}{}
	}
	return setToSlice(set), nil
}

func normalizeAndFilterUSDT(symbols []string) []string {
	out := make([]string, 0, len(symbols))
	seen := make(map[string]struct{})
	for _, s := range symbols {
		s = strings.ToUpper(strings.TrimSpace(s))
		if strings.HasSuffix(s, "_SPBL") {
			s = strings.TrimSuffix(s, "_SPBL")
		}
		if !strings.HasSuffix(s, "USDT") {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func writeLines(path string, lines []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, line := range lines {
		if _, err := w.WriteString(line + "\n"); err != nil {
			return err
		}
	}
	return w.Flush()
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func setToSlice(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
