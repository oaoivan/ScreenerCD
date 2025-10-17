package basepools

import (
	"fmt"
	"regexp"
	"strings"
)

// Version is a normalized representation of the AMM engine version.
type Version string

const (
	VersionUnknown Version = "unknown"
	VersionV1      Version = "v1"
	VersionV2      Version = "v2"
	VersionV3      Version = "v3"
	VersionV4      Version = "v4"
)

var (
	dexSuffixRegexp = regexp.MustCompile(`(?i)_(?:v\d+|amm(?:_[a-z0-9]+)?)$`)
	versionRegexp   = regexp.MustCompile(`^v\d+(?:[a-z0-9_-]+)?$`)
)

// NormalizeDex converts connector name to canonical form (lowercase without trailing "_v3", "_amm", etc.).
func NormalizeDex(raw string) string {
	if raw == "" {
		return ""
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	normalized := strings.ToLower(strings.Join(strings.Fields(trimmed), ""))
	for {
		next := dexSuffixRegexp.ReplaceAllString(normalized, "")
		if next == normalized {
			break
		}
		normalized = next
	}
	return normalized
}

// ParseAMMVersion validates AMM version and falls back to VersionUnknown if the value is not recognised.
func ParseAMMVersion(raw string) (Version, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	if normalized == "" || normalized == string(VersionUnknown) {
		return VersionUnknown, nil
	}
	switch normalized {
	case string(VersionV1), string(VersionV2), string(VersionV3), string(VersionV4):
		return Version(normalized), nil
	}
	if versionRegexp.MatchString(normalized) {
		return Version(normalized), nil
	}
	return VersionUnknown, fmt.Errorf("basepools: unsupported amm_version %q", raw)
}

// Filter represents a set of optional predicates that can be applied to pool entries.
type Filter struct {
	Dexes    []string
	Networks []string
	Symbols  []string
	Versions []Version
	MinUSD   float64
	MaxUSD   float64
}

// Matches returns true when the entry satisfies every non-empty constraint from the filter.
func (e Entry) Matches(f Filter) bool {
	if len(f.Dexes) > 0 {
		targets := make(map[string]struct{}, len(f.Dexes))
		for _, d := range f.Dexes {
			if nd := NormalizeDex(d); nd != "" {
				targets[nd] = struct{}{}
			}
		}
		if len(targets) == 0 {
			return false
		}
		if _, ok := targets[NormalizeDex(e.Dex)]; !ok {
			return false
		}
	}

	if len(f.Networks) > 0 {
		targets := make(map[string]struct{}, len(f.Networks))
		for _, n := range f.Networks {
			nn := strings.ToLower(strings.TrimSpace(n))
			if nn != "" {
				targets[nn] = struct{}{}
			}
		}
		if len(targets) == 0 {
			return false
		}
		if _, ok := targets[strings.ToLower(strings.TrimSpace(e.Network))]; !ok {
			return false
		}
	}

	if len(f.Symbols) > 0 {
		targets := make(map[string]struct{}, len(f.Symbols))
		for _, s := range f.Symbols {
			if normal := normalizeSymbol(s); normal != "" {
				targets[normal] = struct{}{}
			}
		}
		if len(targets) > 0 {
			if _, ok := targets[normalizeSymbol(e.Symbol)]; !ok {
				return false
			}
		}
	}

	if len(f.Versions) > 0 {
		targets := make(map[Version]struct{}, len(f.Versions))
		for _, v := range f.Versions {
			raw := strings.TrimSpace(string(v))
			if raw == "" {
				continue
			}
			normalized, err := ParseAMMVersion(raw)
			if err != nil {
				targets[Version(strings.ToLower(raw))] = struct{}{}
				continue
			}
			targets[normalized] = struct{}{}
		}
		if len(targets) == 0 {
			return false
		}
		entryVersion, err := ParseAMMVersion(e.AMMVersion)
		if err != nil {
			entryVersion = VersionUnknown
		}
		if _, ok := targets[entryVersion]; !ok {
			return false
		}
	}

	if f.MinUSD > 0 && e.LiquidityUSD < f.MinUSD {
		return false
	}
	if f.MaxUSD > 0 && e.LiquidityUSD > f.MaxUSD {
		return false
	}

	return true
}

func normalizeSymbol(raw string) string {
	return strings.ToUpper(strings.TrimSpace(raw))
}
