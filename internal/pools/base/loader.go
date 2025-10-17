package basepools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	cacheMu      sync.RWMutex
	cachedByPath = make(map[string][]Entry)
)

// LoadBasePools reads ticker_source/base_pools.json (or compatible file) and returns parsed entries.
// Results are memoized per absolute path to avoid repeated disk IO when multiple connectors share the same source.
func LoadBasePools(path string) ([]Entry, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, fmt.Errorf("basepools: empty path provided")
	}

	absPath, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, fmt.Errorf("basepools: resolve path: %w", err)
	}

	cacheMu.RLock()
	if entries, ok := cachedByPath[absPath]; ok {
		cacheMu.RUnlock()
		return cloneEntries(entries), nil
	}
	cacheMu.RUnlock()

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("basepools: read %s: %w", absPath, err)
	}

	var payload File
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("basepools: decode %s: %w", absPath, err)
	}
	if len(payload.Entries) == 0 {
		return nil, fmt.Errorf("basepools: %s contains zero entries", absPath)
	}

	entries := cloneEntries(payload.Entries)

	cacheMu.Lock()
	cachedByPath[absPath] = entries
	cacheMu.Unlock()

	return cloneEntries(entries), nil
}

func cloneEntries(src []Entry) []Entry {
	if len(src) == 0 {
		return nil
	}
	dst := make([]Entry, len(src))
	copy(dst, src)
	return dst
}
