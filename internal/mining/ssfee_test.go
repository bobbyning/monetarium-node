// Copyright (c) 2024 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package mining

import (
	"bytes"
	"context"
	"math/big"
	"testing"

	"github.com/monetarium/monetarium-node/blockchain/stake"
	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/chaincfg/chainhash"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/database"
	_ "github.com/monetarium/monetarium-node/database/ffldb"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/internal/blockchain/indexers"
	"github.com/monetarium/monetarium-node/txscript"
	"github.com/monetarium/monetarium-node/wire"
)

// createMockVoteWithConsolidation creates a mock vote with proper consolidation address
// for testing createSSFeeTxBatched. The hash160 is used as the consolidation address.
func createMockVoteWithConsolidation(stakeValue int64, hash160 []byte) *dcrutil.Tx {
	voteTx := wire.NewMsgTx()
	voteTx.Version = 3

	// [0] Block reference (OP_RETURN)
	voteTx.AddTxOut(&wire.TxOut{
		Value:    0,
		CoinType: cointype.CoinTypeVAR,
		PkScript: []byte{txscript.OP_RETURN},
	})

	// [1] Vote bits (OP_RETURN)
	voteTx.AddTxOut(&wire.TxOut{
		Value:    0,
		CoinType: cointype.CoinTypeVAR,
		PkScript: []byte{txscript.OP_RETURN},
	})

	// [2] Reward output
	voteTx.AddTxOut(&wire.TxOut{
		Value:    stakeValue,
		CoinType: cointype.CoinTypeVAR,
		Version:  0,
		PkScript: []byte{txscript.OP_DUP, txscript.OP_HASH160, 0x14},
	})

	// [3] Consolidation address (OP_RETURN + "SC" + hash160)
	consolidationOut, err := stake.CreateSSFeeConsolidationOutput(hash160)
	if err != nil {
		// Use default hash160 if provided one is invalid
		defaultHash := make([]byte, 20)
		consolidationOut, _ = stake.CreateSSFeeConsolidationOutput(defaultHash)
	}
	voteTx.AddTxOut(consolidationOut)

	return dcrutil.NewTx(voteTx)
}

// TestCreateSSFeeTxBatched tests the createSSFeeTxBatched function with various inputs
func TestCreateSSFeeTxBatched(t *testing.T) {
	// Create mock voters with same consolidation address (will be batched into 1 tx)
	sameAddrHash := make([]byte, 20)
	sameAddrHash[0] = 0x01 // Unique address
	votersSameAddr := make([]*dcrutil.Tx, 3)
	for i := range votersSameAddr {
		votersSameAddr[i] = createMockVoteWithConsolidation(1000, sameAddrHash)
	}

	// Create mock voters with different consolidation addresses (one tx per address)
	votersDiffAddr := make([]*dcrutil.Tx, 3)
	for i := range votersDiffAddr {
		hash := make([]byte, 20)
		hash[0] = byte(i + 1) // Different addresses
		votersDiffAddr[i] = createMockVoteWithConsolidation(1000, hash)
	}

	tests := []struct {
		name           string
		coinType       cointype.CoinType
		totalFee       *big.Int
		voters         []*dcrutil.Tx
		height         int64
		expectError    bool
		errorMsg       string
		expectedTxns   int  // Number of SSFee transactions expected
		expectNilEmpty bool // Expect nil/empty result (no error, no txs)
	}{
		{
			name:         "valid SKA1 fee distribution - same address",
			coinType:     1,
			totalFee:     big.NewInt(3000),
			voters:       votersSameAddr,
			height:       100,
			expectError:  false,
			expectedTxns: 1, // All voters have same address = 1 batched tx
		},
		{
			name:         "valid SKA1 fee distribution - different addresses",
			coinType:     1,
			totalFee:     big.NewInt(3000),
			voters:       votersDiffAddr,
			height:       100,
			expectError:  false,
			expectedTxns: 3, // Each voter has different address = 3 txs
		},
		{
			name:         "valid SKA2 fee distribution",
			coinType:     2,
			totalFee:     big.NewInt(6000),
			voters:       votersDiffAddr,
			height:       200,
			expectError:  false,
			expectedTxns: 3,
		},
		{
			name:         "valid VAR fee distribution",
			coinType:     cointype.CoinTypeVAR,
			totalFee:     big.NewInt(1000),
			voters:       votersDiffAddr,
			height:       100,
			expectError:  false,
			expectedTxns: 3,
		},
		{
			name:           "no voters",
			coinType:       1,
			totalFee:       big.NewInt(1000),
			voters:         []*dcrutil.Tx{},
			height:         100,
			expectError:    false, // Batched returns nil, nil for no voters
			expectNilEmpty: true,
		},
		{
			name:           "zero total fee",
			coinType:       1,
			totalFee:       big.NewInt(0),
			voters:         votersDiffAddr,
			height:         100,
			expectError:    false,
			expectNilEmpty: true, // Zero fee results in nil/empty
		},
		{
			name:         "remainder distribution test",
			coinType:     1,
			totalFee:     big.NewInt(10), // 10 atoms to 3 voters = 3 each + 1 remainder to first sorted
			voters:       votersDiffAddr,
			height:       100,
			expectError:  false,
			expectedTxns: 3,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ssFeeTxns, err := createSSFeeTxBatched(test.coinType, test.totalFee,
				test.voters, test.height, nil, nil, nil, nil)

			if test.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				} else if test.errorMsg != "" && !contains(err.Error(), test.errorMsg) {
					t.Errorf("Expected error containing '%s' but got '%s'", test.errorMsg, err.Error())
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if test.expectNilEmpty {
				if len(ssFeeTxns) != 0 {
					t.Errorf("Expected nil/empty result, got %d transactions", len(ssFeeTxns))
				}
				return
			}

			if len(ssFeeTxns) != test.expectedTxns {
				t.Errorf("Expected %d SSFee transactions, got %d", test.expectedTxns, len(ssFeeTxns))
				return
			}

			// Verify each SSFee transaction
			var totalDistributed int64
			for txIdx, ssFeeTx := range ssFeeTxns {
				msgTx := ssFeeTx.MsgTx()

				// Check transaction version
				if msgTx.Version != 3 {
					t.Errorf("Tx %d: Expected version 3, got %d", txIdx, msgTx.Version)
				}

				// Check it's a stake transaction
				if ssFeeTx.Tree() != wire.TxTreeStake {
					t.Errorf("Tx %d: Expected stake tree transaction", txIdx)
				}

				// Check it has exactly one input (null input for new UTXO)
				if len(msgTx.TxIn) != 1 {
					t.Errorf("Tx %d: Expected 1 input, got %d", txIdx, len(msgTx.TxIn))
				}

				// Should be null input since we passed nil ssfeeIndex
				if msgTx.TxIn[0].PreviousOutPoint.Index != wire.MaxPrevOutIndex {
					t.Errorf("Tx %d: Expected null input", txIdx)
				}

				// Check outputs: one OP_RETURN marker + one payment output
				if len(msgTx.TxOut) != 2 {
					t.Errorf("Tx %d: Expected 2 outputs, got %d", txIdx, len(msgTx.TxOut))
				}

				// Check payment output (index 1) has the correct coin type
				if msgTx.TxOut[1].CoinType != test.coinType {
					t.Errorf("Tx %d: Output has coin type %d, expected %d",
						txIdx, msgTx.TxOut[1].CoinType, test.coinType)
				}

				// Accumulate total distributed (payment is at index 1)
				// VAR uses Value, SKA uses SKAValue
				if msgTx.TxOut[1].CoinType.IsSKA() {
					if msgTx.TxOut[1].SKAValue != nil {
						totalDistributed += msgTx.TxOut[1].SKAValue.Int64()
					}
				} else {
					totalDistributed += msgTx.TxOut[1].Value
				}
			}

			// Verify total distributed across all transactions equals input fee
			if totalDistributed != test.totalFee.Int64() {
				t.Errorf("Total distributed %d != total fee %v",
					totalDistributed, test.totalFee)
			}
		})
	}
}

// TestSSFeeMultipleCoinTypes tests that multiple SSFee transactions can be created
// for different coin types in the same block
func TestSSFeeMultipleCoinTypes(t *testing.T) {
	// Create mock voters with different consolidation addresses
	voters := make([]*dcrutil.Tx, 2)
	for i := range voters {
		hash := make([]byte, 20)
		hash[0] = byte(i + 1) // Different addresses
		voters[i] = createMockVoteWithConsolidation(1000*int64(i+1), hash)
	}

	// Test creating SSFee for multiple coin types
	// These are the staker portions (50% of total fees per coin type)
	coinTypes := []cointype.CoinType{1, 2, 3}                                // SKA1, SKA2, SKA3
	fees := []*big.Int{big.NewInt(2000), big.NewInt(4000), big.NewInt(6000)} // Staker portions after 50/50 split

	allSSFeeTxns := make([][]*dcrutil.Tx, 0)
	for i, coinType := range coinTypes {
		ssFeeTxns, err := createSSFeeTxBatched(coinType, fees[i], voters, 100, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("Failed to create SSFee for coin type %d: %v", coinType, err)
		}
		allSSFeeTxns = append(allSSFeeTxns, ssFeeTxns)
	}

	// Verify each SSFee transaction group
	for i, ssFeeTxns := range allSSFeeTxns {
		expectedCoinType := coinTypes[i]
		var totalDistributed int64

		// Should have 2 txs (one per voter with different address)
		if len(ssFeeTxns) != 2 {
			t.Errorf("SSFee group %d: expected 2 transactions, got %d", i, len(ssFeeTxns))
		}

		for _, ssFeeTx := range ssFeeTxns {
			msgTx := ssFeeTx.MsgTx()

			// Check payment output (index 1) has correct coin type
			if msgTx.TxOut[1].CoinType != expectedCoinType {
				t.Errorf("SSFee %d has wrong coin type: got %d, want %d",
					i, msgTx.TxOut[1].CoinType, expectedCoinType)
			}
			// SKA outputs use SKAValue, not Value
			if msgTx.TxOut[1].SKAValue != nil {
				totalDistributed += msgTx.TxOut[1].SKAValue.Int64()
			}
		}

		// Check fee distribution across all SSFee transactions for this coin type
		if totalDistributed != fees[i].Int64() {
			t.Errorf("SSFee group %d distributed wrong amount: got %d, want %v",
				i, totalDistributed, fees[i])
		}
	}

	// Verify all SSFee transactions are different
	hashes := make(map[chainhash.Hash]bool)
	for _, ssFeeTxns := range allSSFeeTxns {
		for _, ssFeeTx := range ssFeeTxns {
			hash := ssFeeTx.Hash()
			if hashes[*hash] {
				t.Errorf("Duplicate SSFee transaction hash: %v", hash)
			}
			hashes[*hash] = true
		}
	}
}

// TestSSFeeEdgeCases tests various edge cases and security scenarios for createSSFeeTxBatched
func TestSSFeeEdgeCases(t *testing.T) {
	t.Run("malformed voters - missing consolidation address", func(t *testing.T) {
		// Create voter without consolidation address output
		voteTx := wire.NewMsgTx()
		voteTx.Version = 3
		voteTx.AddTxOut(&wire.TxOut{Value: 0, CoinType: cointype.CoinTypeVAR})
		voteTx.AddTxOut(&wire.TxOut{Value: 0, CoinType: cointype.CoinTypeVAR})
		voteTx.AddTxOut(&wire.TxOut{Value: 1000, CoinType: cointype.CoinTypeVAR})
		// Missing consolidation address output
		voters := []*dcrutil.Tx{dcrutil.NewTx(voteTx)}

		_, err := createSSFeeTxBatched(1, big.NewInt(1000), voters, 100, nil, nil, nil, nil)
		if err == nil {
			t.Errorf("Expected error for missing consolidation address")
		}
	})

	t.Run("remainder distribution to first sorted address", func(t *testing.T) {
		// Create 3 voters with different consolidation addresses
		// Address order when sorted: 0x01 < 0x02 < 0x03
		voters := make([]*dcrutil.Tx, 3)
		hashes := [][]byte{
			{0x03, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, // Will be third when sorted
			{0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, // Will be first when sorted
			{0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, // Will be second when sorted
		}
		for i, hash := range hashes {
			voters[i] = createMockVoteWithConsolidation(1000, hash)
		}

		// 100 atoms / 3 voters = 33 each + 1 remainder
		ssFeeTxns, err := createSSFeeTxBatched(1, big.NewInt(100), voters, 100, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if len(ssFeeTxns) != 3 {
			t.Fatalf("Expected 3 transactions, got %d", len(ssFeeTxns))
		}

		// Total should be exactly 100
		var total int64
		for _, tx := range ssFeeTxns {
			// SKA outputs use SKAValue, not Value
			if tx.MsgTx().TxOut[1].SKAValue != nil {
				total += tx.MsgTx().TxOut[1].SKAValue.Int64()
			}
		}
		if total != 100 {
			t.Errorf("Total distributed %d != 100", total)
		}
	})

	t.Run("batching multiple voters same address", func(t *testing.T) {
		// Create 5 voters all with same consolidation address
		hash := make([]byte, 20)
		hash[0] = 0x42
		voters := make([]*dcrutil.Tx, 5)
		for i := range voters {
			voters[i] = createMockVoteWithConsolidation(1000, hash)
		}

		ssFeeTxns, err := createSSFeeTxBatched(1, big.NewInt(5000), voters, 100, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		// Should produce only 1 transaction (all voters batched)
		if len(ssFeeTxns) != 1 {
			t.Errorf("Expected 1 batched transaction, got %d", len(ssFeeTxns))
		}

		// Single tx should have full 5000
		// SKA outputs use SKAValue, not Value
		var outputValue int64
		if ssFeeTxns[0].MsgTx().TxOut[1].SKAValue != nil {
			outputValue = ssFeeTxns[0].MsgTx().TxOut[1].SKAValue.Int64()
		}
		if outputValue != 5000 {
			t.Errorf("Expected 5000, got %d", outputValue)
		}
	})

	t.Run("mixed consolidation addresses", func(t *testing.T) {
		// 3 voters: 2 with same address, 1 with different
		sameHash := make([]byte, 20)
		sameHash[0] = 0x01
		diffHash := make([]byte, 20)
		diffHash[0] = 0x02

		voters := []*dcrutil.Tx{
			createMockVoteWithConsolidation(1000, sameHash),
			createMockVoteWithConsolidation(2000, sameHash), // Same as voter 0
			createMockVoteWithConsolidation(3000, diffHash), // Different
		}

		// 3000 total fee / 3 votes = 1000 per vote
		// Group 1 (sameHash, 2 votes): 2000
		// Group 2 (diffHash, 1 vote): 1000
		ssFeeTxns, err := createSSFeeTxBatched(1, big.NewInt(3000), voters, 100, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		// Should produce 2 transactions (one per unique address)
		if len(ssFeeTxns) != 2 {
			t.Errorf("Expected 2 transactions, got %d", len(ssFeeTxns))
		}

		// Total should be 3000
		var total int64
		for _, tx := range ssFeeTxns {
			// SKA outputs use SKAValue, not Value
			if tx.MsgTx().TxOut[1].SKAValue != nil {
				total += tx.MsgTx().TxOut[1].SKAValue.Int64()
			}
		}
		if total != 3000 {
			t.Errorf("Total distributed %d != 3000", total)
		}
	})
}

// contains checks if a string contains a substring (helper function)
func contains(s, substr string) bool {
	return len(substr) == 0 || len(s) >= len(substr) &&
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}()
}

// TestSSFeeIntegration tests that SSFee transactions integrate properly with
// the block template generation
func TestSSFeeIntegration(t *testing.T) {
	// This test would require a more complete setup with a mock BlockChain
	// and would test the full integration in NewBlockTemplate
	t.Skip("Integration test requires full mining harness setup")
}

// TestCreateSSFeeTxBatchedUTXOAugmentation tests UTXO augmentation for staker fees
func TestCreateSSFeeTxBatchedUTXOAugmentation(t *testing.T) {
	// NOTE: This test demonstrates the augmentation logic but cannot fully test it
	// without a complete SSFeeIndex implementation. The test shows that:
	// 1. Without ssfeeIndex, SSFee transactions use null inputs (create new UTXOs)
	// 2. With ssfeeIndex (in production), SSFee can augment existing UTXOs

	// Create mock voters with distinct consolidation addresses
	voters := make([]*dcrutil.Tx, 3)
	hashes := [][]byte{
		{0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		{0x03, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	}
	for i, hash := range hashes {
		voters[i] = createMockVoteWithConsolidation(1000, hash)
	}

	t.Run("no ssfeeIndex - creates new UTXOs", func(t *testing.T) {
		ssFeeTxns, err := createSSFeeTxBatched(1, big.NewInt(3000), voters, 100, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		// Should create 3 transactions (one per unique consolidation address)
		if len(ssFeeTxns) != 3 {
			t.Fatalf("Expected 3 SSFee transactions, got %d", len(ssFeeTxns))
		}

		// All should have null inputs (no augmentation)
		for i, tx := range ssFeeTxns {
			if len(tx.MsgTx().TxIn) != 1 {
				t.Errorf("Tx %d: Expected 1 input", i)
			}
			if tx.MsgTx().TxIn[0].PreviousOutPoint.Index != wire.MaxPrevOutIndex {
				t.Errorf("Tx %d: Expected null input (no UTXO to augment)", i)
			}
			// Output value should equal fee (1000 each for 3 addresses)
			// SKA outputs use SKAValue, not Value
			var outputValue int64
			if tx.MsgTx().TxOut[1].SKAValue != nil {
				outputValue = tx.MsgTx().TxOut[1].SKAValue.Int64()
			}
			if outputValue != 1000 {
				t.Errorf("Tx %d: Expected output value 1000, got %d", i, outputValue)
			}
		}
	})

	t.Run("multiple rounds accumulate fees", func(t *testing.T) {
		// Round 1: Create initial SSFee transactions
		round1Txns, err := createSSFeeTxBatched(1, big.NewInt(3000), voters, 100, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("Round 1 error: %v", err)
		}

		// Round 2: Create more SSFee transactions (simulating next block)
		// In real scenario with SSFeeIndex, round2 would augment round1 outputs
		round2Txns, err := createSSFeeTxBatched(1, big.NewInt(3000), voters, 101, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("Round 2 error: %v", err)
		}

		// Both rounds should create separate transactions (no index = no augmentation)
		if len(round1Txns) != 3 || len(round2Txns) != 3 {
			t.Fatalf("Expected 3 transactions per round")
		}

		// Without augmentation: 6 total UTXOs (dust)
		// With augmentation: would have 3 UTXOs (one per address, augmented)
		totalUTXOs := len(round1Txns) + len(round2Txns)
		t.Logf("Without augmentation: %d total UTXOs created", totalUTXOs)
		t.Logf("With augmentation: would create only %d UTXOs", len(voters))
	})

	t.Run("different coin types don't interfere", func(t *testing.T) {
		// Create SSFee for SKA1
		ska1Txns, err := createSSFeeTxBatched(1, big.NewInt(3000), voters, 100, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("SKA1 error: %v", err)
		}

		// Create SSFee for SKA2
		ska2Txns, err := createSSFeeTxBatched(2, big.NewInt(6000), voters, 100, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("SKA2 error: %v", err)
		}

		// Should create separate transactions per coin type
		if len(ska1Txns) != 3 || len(ska2Txns) != 3 {
			t.Fatalf("Expected 3 transactions per coin type")
		}

		// Verify coin types are correct (payment output is at index 1)
		for _, tx := range ska1Txns {
			if tx.MsgTx().TxOut[1].CoinType != 1 {
				t.Errorf("SKA1 transaction has wrong coin type")
			}
		}
		for _, tx := range ska2Txns {
			if tx.MsgTx().TxOut[1].CoinType != 2 {
				t.Errorf("SKA2 transaction has wrong coin type")
			}
			// SKA2 should have 2000 per address (6000 / 3)
			// SKA outputs use SKAValue, not Value
			var outputValue int64
			if tx.MsgTx().TxOut[1].SKAValue != nil {
				outputValue = tx.MsgTx().TxOut[1].SKAValue.Int64()
			}
			if outputValue != 2000 {
				t.Errorf("SKA2 tx has wrong value: %d", outputValue)
			}
		}
	})

	t.Run("each transaction has unique OP_RETURN", func(t *testing.T) {
		ssFeeTxns, err := createSSFeeTxBatched(1, big.NewInt(3000), voters, 100, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		// Collect all OP_RETURN scripts (at index 0)
		opReturns := make([][]byte, len(ssFeeTxns))
		for i, tx := range ssFeeTxns {
			opReturns[i] = tx.MsgTx().TxOut[0].PkScript
		}

		// All should be unique
		for i := 0; i < len(opReturns); i++ {
			for j := i + 1; j < len(opReturns); j++ {
				if string(opReturns[i]) == string(opReturns[j]) {
					t.Errorf("SSFee transactions %d and %d have identical OP_RETURN", i, j)
				}
			}
		}
	})
}

// ssFeeScriptHashVote returns a mock vote whose consolidation address is the
// P2SH ("SH") address of hash160.
func ssFeeScriptHashVote(t *testing.T, hash160 []byte) *dcrutil.Tx {
	t.Helper()
	voteTx := createMockVoteWithConsolidation(1000, hash160).MsgTx()
	out, err := stake.CreateSSFeeScriptHashConsolidationOutput(hash160)
	if err != nil {
		t.Fatal(err)
	}
	voteTx.TxOut[3] = out
	return dcrutil.NewTx(voteTx)
}

// TestCreateSSFeeTxBatchedScriptHash ensures voters with a P2SH ("SH")
// consolidation address are paid the OP_SSGEN-tagged P2SH script of that
// address, in a group of their own apart from P2PKH voters of the same
// hash160, and that groups are ordered by script: every P2PKH script sorts
// before every P2SH script, so a P2PKH group takes the remainder even when a
// P2SH group has the smaller hash160.
func TestCreateSSFeeTxBatchedScriptHash(t *testing.T) {
	addr := bytes.Repeat([]byte{0x01}, 20)
	highAddr := bytes.Repeat([]byte{0x02}, 20)
	p2shVote := ssFeeScriptHashVote(t, addr)

	p2pkhScript, _ := stake.ConsolidationAddrToPkScript(addr)
	p2shScript, _ := stake.ConsolidationScriptHashToPkScript(addr)
	highP2PKHScript, _ := stake.ConsolidationAddrToPkScript(highAddr)
	type payout struct {
		script []byte
		amount int64
	}
	tests := []struct {
		name   string
		voters []*dcrutil.Tx
		want   []payout
	}{{
		name: "same hash160",
		voters: []*dcrutil.Tx{p2shVote,
			createMockVoteWithConsolidation(1000, addr), p2shVote},
		want: []payout{{p2pkhScript, 1001}, {p2shScript, 2000}},
	}, {
		name: "P2SH voter with the smaller hash160",
		voters: []*dcrutil.Tx{p2shVote,
			createMockVoteWithConsolidation(1000, highAddr)},
		want: []payout{{highP2PKHScript, 1501}, {p2shScript, 1500}},
	}}

	for _, test := range tests {
		for _, coinType := range []cointype.CoinType{cointype.CoinTypeVAR, 1} {
			txns, err := createSSFeeTxBatched(coinType, big.NewInt(3001),
				test.voters, 100, nil, nil, nil, nil)
			if err != nil {
				t.Fatalf("%s/%v: unexpected error: %v", test.name, coinType, err)
			}
			if len(txns) != len(test.want) {
				t.Fatalf("%s/%v: got %d SSFee transactions, want %d", test.name,
					coinType, len(txns), len(test.want))
			}
			for i, tx := range txns {
				payOut := tx.MsgTx().TxOut[1]
				if !bytes.Equal(payOut.PkScript, test.want[i].script) ||
					payOut.Version != 0 {

					t.Errorf("%s/%v: SSFee %d pays script %x version %d, want "+
						"%x version 0", test.name, coinType, i, payOut.PkScript,
						payOut.Version, test.want[i].script)
				}
				if payOut.CoinType != coinType {
					t.Errorf("%s/%v: SSFee %d pays coin type %v", test.name,
						coinType, i, payOut.CoinType)
				}
				amount := big.NewInt(payOut.Value)
				if coinType.IsSKA() {
					amount = payOut.SKAValue
				}
				if amount.Cmp(big.NewInt(test.want[i].amount)) != 0 {
					t.Errorf("%s/%v: SSFee %d pays %v, want %d", test.name,
						coinType, i, amount, test.want[i].amount)
				}
			}
		}
	}
}

// ssFeeIndexTestChain is the chain of an SSFee index in tests.  The index
// only needs the chain parameters to start on an empty database.
type ssFeeIndexTestChain struct {
	indexers.ChainQueryer
}

func (ssFeeIndexTestChain) ChainParams() *chaincfg.Params {
	return chaincfg.SimNetParams()
}

// viewCountDB counts the read transactions on a database, which is how an
// SSFee index lookup starts.
type viewCountDB struct {
	database.DB
	views int
}

func (db *viewCountDB) View(fn func(database.Tx) error) error {
	db.views++
	return db.DB.View(fn)
}

// TestCreateSSFeeTxBatchedScriptHashNoIndexLookup ensures the generator does
// not look up the SSFee index for a P2SH group, whose outputs the index never
// files, and does look it up for a P2PKH group of the same hash160.
func TestCreateSSFeeTxBatchedScriptHashNoIndexLookup(t *testing.T) {
	ffldb, err := database.Create("ffldb", t.TempDir(), wire.SimNet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ffldb.Close() })
	db := &viewCountDB{DB: ffldb}
	subscriber := indexers.NewIndexSubscriber(context.Background())
	idx, err := indexers.NewSSFeeIndex(subscriber, db, ssFeeIndexTestChain{})
	if err != nil {
		t.Fatal(err)
	}

	addr := bytes.Repeat([]byte{0x01}, 20)
	tests := []struct {
		name      string
		voter     *dcrutil.Tx
		wantViews bool
	}{{
		name:  "P2SH group",
		voter: ssFeeScriptHashVote(t, addr),
	}, {
		name:      "P2PKH group",
		voter:     createMockVoteWithConsolidation(1000, addr),
		wantViews: true,
	}}

	for _, test := range tests {
		for _, coinType := range []cointype.CoinType{cointype.CoinTypeVAR, 1} {
			db.views = 0
			txns, err := createSSFeeTxBatched(coinType, big.NewInt(3001),
				[]*dcrutil.Tx{test.voter}, 100, idx, nil, nil, nil)
			if err != nil {
				t.Fatalf("%s/%v: unexpected error: %v", test.name, coinType, err)
			}
			if len(txns) != 1 {
				t.Fatalf("%s/%v: got %d SSFee transactions, want 1", test.name,
					coinType, len(txns))
			}
			if gotViews := db.views > 0; gotViews != test.wantViews {
				t.Errorf("%s/%v: index looked up %d time(s), want lookup %v",
					test.name, coinType, db.views, test.wantViews)
			}
		}
	}
}
