// Copyright (c) 2024 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"testing"

	"github.com/monetarium/monetarium-node/cointype"
)

// TestMakeDistributionKey tests the makeDistributionKey helper function.
func TestMakeDistributionKey(t *testing.T) {
	tests := []struct {
		name        string
		coinType    cointype.CoinType
		hash160Hex  string
		expectedKey string
	}{
		{
			name:        "VAR coin type",
			coinType:    cointype.CoinTypeVAR,
			hash160Hex:  "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b",
			expectedKey: "0_1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b",
		},
		{
			name:        "SKA1 coin type",
			coinType:    cointype.CoinType(1),
			hash160Hex:  "abcdef1234567890abcdef1234567890abcdef12",
			expectedKey: "1_abcdef1234567890abcdef1234567890abcdef12",
		},
		{
			name:        "SKA2 coin type",
			coinType:    cointype.CoinType(2),
			hash160Hex:  "0000000000000000000000000000000000000000",
			expectedKey: "2_0000000000000000000000000000000000000000",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := makeDistributionKey(tc.coinType, tc.hash160Hex)
			if result != tc.expectedKey {
				t.Errorf("Expected key %s, got %s", tc.expectedKey, result)
			}
		})
	}
}
