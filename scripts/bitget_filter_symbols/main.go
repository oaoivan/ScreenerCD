//go:build ignore

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

// Offline helper: fetch Bitget SPOT markets, intersect with base USDT symbols,
// and store the overlap in Temp/bitget_usdt_intersection.txt.
func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	baseFile := "Temp/all_contracts_merged_reformatted.json"
	outFile := "Temp/bitget_usdt_intersection.txt"

	baseSymbols, err := util.LoadSymbolsFromFile(baseFile)
	if err != nil {
		log.Fatalf("[FATAL] load base symbols: %v", err)
	}
	log.Printf("[INFO] base symbols loaded: %d", len(baseSymbols))

	baseSet := make(map[string]struct{}, len(baseSymbols))
	for _, s := range baseSymbols {
		baseSet[strings.ToUpper(strings.TrimSpace(s))] = struct{}{}
	}

	bitgetSymbols, err := fetchBitgetSpotUSDT()
	if err != nil {
		log.Fatalf("[FATAL] fetch bitget symbols: %v", err)
	}
	log.Printf("[INFO] bitget spot USDT symbols: %d", len(bitgetSymbols))

	var intersection []string
	for _, s := range bitgetSymbols {
		if _, ok := baseSet[s]; ok {
			intersection = append(intersection, s)
		}
	}
	sort.Strings(intersection)
	log.Printf("[INFO] intersection size: %d", len(intersection))

	if err := writeLines(outFile, intersection); err != nil {
		log.Fatalf("[FATAL] write output: %v", err)
	}
	log.Printf("[INFO] written %d symbols to %s", len(intersection), outFile)
}

func fetchBitgetSpotUSDT() ([]string, error) {
	client := &http.Client{Timeout: 20 * time.Second}

	v2URL := "https://api.bitget.com/api/v2/spot/public/symbols"
	if symbols, err := requestBitgetSymbolsV2(client, v2URL); err == nil && len(symbols) > 0 {
		return normalizeAndFilterUSDT(symbols), nil
	}

	v1URL := "https://api.bitget.com/api/spot/v1/public/symbols"
	symbols, err := requestBitgetSymbolsV1(client, v1URL)
	if err != nil {
		return nil, fmt.Errorf("bitget v1 request failed: %w", err)
	}
	return normalizeAndFilterUSDT(symbols), nil
}

func requestBitgetSymbolsV2(client *http.Client, url string) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body))
	}
	var payload struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Symbol    string `json:"symbol"`
			InstID    string `json:"instId"`
			BaseCoin  string `json:"baseCoin"`
			QuoteCoin string `json:"quoteCoin"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if payload.Code != "00000" {
		return nil, fmt.Errorf("api code=%s msg=%s", payload.Code, payload.Msg)
	}
	set := make(map[string]struct{})
	for _, item := range payload.Data {
		symbol := strings.ToUpper(strings.TrimSpace(firstNonEmpty(item.Symbol, item.InstID)))
		if symbol == "" && item.BaseCoin != "" && item.QuoteCoin != "" {
			symbol = strings.ToUpper(item.BaseCoin + item.QuoteCoin)
		}
		if symbol == "" {
			continue
		}
		set[symbol] = struct{}{}
	}
	return setToSlice(set), nil
}

func requestBitgetSymbolsV1(client *http.Client, url string) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body))
	}
	var payload struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Symbol    string `json:"symbol"`
			BaseCoin  string `json:"baseCoin"`
			QuoteCoin string `json:"quoteCoin"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if payload.Code != "00000" {
		return nil, fmt.Errorf("api code=%s msg=%s", payload.Code, payload.Msg)
	}
	set := make(map[string]struct{})
	for _, item := range payload.Data {
		symbol := strings.ToUpper(strings.TrimSpace(item.Symbol))
		if symbol == "" && item.BaseCoin != "" && item.QuoteCoin != "" {
			symbol = strings.ToUpper(item.BaseCoin + item.QuoteCoin)
		}
		if strings.HasSuffix(symbol, "_SPBL") {
			symbol = strings.TrimSuffix(symbol, "_SPBL")
		}
		if symbol == "" {
			continue
		}
		set[symbol] = struct{}{}
	}
	return setToSlice(set), nil
}

func normalizeAndFilterUSDT(symbols []string) []string {
	set := make(map[string]struct{})
	for _, s := range symbols {
		normalized := strings.ToUpper(strings.TrimSpace(s))
		if strings.HasSuffix(normalized, "_SPBL") {
			normalized = strings.TrimSuffix(normalized, "_SPBL")
		}
		if !strings.HasSuffix(normalized, "USDT") {
			continue
		}
		set[normalized] = struct{}{}
	}
	return setToSlice(set)
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
