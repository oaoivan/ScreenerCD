package baseuniswap

import "testing"

func TestNormalizePair(t *testing.T) {
	tests := []struct {
		name     string
		current  string
		token0   string
		token1   string
		expected string
	}{
		{
			name:     "empty uses token symbols",
			current:  "",
			token0:   "eth",
			token1:   "usdc",
			expected: "ETH/USDC",
		},
		{
			name:     "existing value trimmed",
			current:  " weth /  usdc ",
			token0:   "ignored",
			token1:   "ignored",
			expected: "WETH /  USDC",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizePair(tt.current, tt.token0, tt.token1)
			if got != tt.expected {
				t.Fatalf("normalizePair(%q,%q,%q) = %q want %q", tt.current, tt.token0, tt.token1, got, tt.expected)
			}
		})
	}
}
