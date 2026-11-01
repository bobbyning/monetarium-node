// Copyright (c) 2024 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package stake

import (
	"errors"
	"testing"

	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/wire"
)

// Test hash160 for consolidation address
var ssgenConsolidationHash160 = []byte{
	0x1a, 0x2b, 0x3c, 0x4d, 0x5e, 0x6f, 0x7a, 0x8b, 0x9c, 0x0d,
	0x1e, 0x2f, 0x3a, 0x4b, 0x5c, 0x6d, 0x7e, 0x8f, 0x9a, 0x0b,
}

// buildConsolidationOutput creates a valid consolidation address output.
func buildConsolidationOutput() *wire.TxOut {
	output, err := CreateSSFeeConsolidationOutput(ssgenConsolidationHash160)
	if err != nil {
		panic("failed to create test consolidation output: " + err.Error())
	}
	return output
}

// ssgenMsgTxWithConsolidation is a valid SSGen MsgTx with consolidation address.
var ssgenMsgTxWithConsolidation = &wire.MsgTx{
	SerType: wire.TxSerializeFull,
	Version: 1,
	TxIn: []*wire.TxIn{
		&ssgenTxIn0, // stakebase
		&ssgenTxIn1, // ticket reference
	},
	TxOut: []*wire.TxOut{
		&ssgenTxOut0,               // [0] block reference
		&ssgenTxOut1,               // [1] vote bits
		&ssgenTxOut2,               // [2] reward 1
		&ssgenTxOut3,               // [3] reward 2
		buildConsolidationOutput(), // [4] consolidation address
	},
	LockTime: 0,
	Expiry:   0,
}

// ssgenMsgTxWithConsolidationAtPos2 has consolidation immediately after vote bits.
var ssgenMsgTxWithConsolidationAtPos2 = &wire.MsgTx{
	SerType: wire.TxSerializeFull,
	Version: 1,
	TxIn: []*wire.TxIn{
		&ssgenTxIn0,
		&ssgenTxIn1,
	},
	TxOut: []*wire.TxOut{
		&ssgenTxOut0,               // [0] block reference
		&ssgenTxOut1,               // [1] vote bits
		buildConsolidationOutput(), // [2] consolidation address
		&ssgenTxOut2,               // [3] reward 1
		&ssgenTxOut3,               // [4] reward 2
	},
	LockTime: 0,
	Expiry:   0,
}

// ssgenMsgTxWithScriptHashConsolidation has a P2SH ("SH") consolidation address.
var ssgenMsgTxWithScriptHashConsolidation = func() *wire.MsgTx {
	output, err := CreateSSFeeScriptHashConsolidationOutput(ssgenConsolidationHash160)
	if err != nil {
		panic("failed to create test consolidation output: " + err.Error())
	}
	tx := ssgenMsgTxWithConsolidation.Copy()
	tx.TxOut[4] = output
	return tx
}()

// ssgenMsgTxWithScriptHashConsolidationAndTreasury has a P2SH consolidation
// address followed by a treasury vote.
var ssgenMsgTxWithScriptHashConsolidationAndTreasury = func() *wire.MsgTx {
	tx := ssgenMsgTxWithConsolidationAndTreasury.Copy()
	tx.TxOut[4] = ssgenMsgTxWithScriptHashConsolidation.TxOut[4]
	return tx
}()

// ssgenMsgTxWithBothConsolidations has a P2SH consolidation address and then
// a P2PKH one.
var ssgenMsgTxWithBothConsolidations = func() *wire.MsgTx {
	tx := ssgenMsgTxWithScriptHashConsolidation.Copy()
	tx.TxOut = append(tx.TxOut, buildConsolidationOutput())
	return tx
}()

// ssgenMsgTxWithMalformedScriptHash returns ssgenMsgTxWithScriptHashConsolidation
// with its "SH" output script changed by mutate.  When withSC is true, the
// malformed output follows a valid "SC" output instead of replacing it.
func ssgenMsgTxWithMalformedScriptHash(withSC bool, mutate func([]byte) []byte) *wire.MsgTx {
	sh := ssgenMsgTxWithScriptHashConsolidation.TxOut[4]
	malformed := &wire.TxOut{
		Value:    sh.Value,
		CoinType: sh.CoinType,
		Version:  sh.Version,
		PkScript: mutate(append([]byte(nil), sh.PkScript...)),
	}
	if withSC {
		tx := ssgenMsgTxWithConsolidation.Copy()
		tx.TxOut = append(tx.TxOut, malformed)
		return tx
	}
	tx := ssgenMsgTxWithScriptHashConsolidation.Copy()
	tx.TxOut[4] = malformed
	return tx
}

// Malformed "SH" consolidation outputs, each one property away from a valid one.
var (
	shWrongPush         = func(s []byte) []byte { s[1] = 0x15; return s } // OP_DATA_21
	shNoOpReturn        = func(s []byte) []byte { s[0] = 0x00; return s } // OP_0
	shWrongLength       = func(s []byte) []byte { return s[:23] }
	shTooLong           = func(s []byte) []byte { return append(s, 0x00) }
	shWrongFirstMarker  = func(s []byte) []byte { s[2] = 0x99; return s } // 0x99 'H'
	shWrongSecondMarker = func(s []byte) []byte { s[3] = 0x99; return s } // 'S' 0x99
)

// ssgenMsgTxWithConsolidationAndTreasury has both consolidation and treasury vote.
var ssgenMsgTxWithConsolidationAndTreasury = &wire.MsgTx{
	SerType: wire.TxSerializeFull,
	Version: wire.TxVersionTreasury,
	TxIn: []*wire.TxIn{
		&ssgenTxIn0,
		&ssgenTxIn1,
	},
	TxOut: []*wire.TxOut{
		&ssgenTxOut0,               // [0] block reference
		&ssgenTxOut1,               // [1] vote bits
		&ssgenTxOut2,               // [2] reward 1
		&ssgenTxOut3,               // [3] reward 2
		buildConsolidationOutput(), // [4] consolidation address
		&ssgenTxOutValidTV,         // [5] treasury vote (must be last)
	},
	LockTime: 0,
	Expiry:   0,
}

// ssgenMsgTxNoConsolidation is an invalid SSGen MsgTx missing consolidation address.
var ssgenMsgTxNoConsolidation = &wire.MsgTx{
	SerType: wire.TxSerializeFull,
	Version: 1,
	TxIn: []*wire.TxIn{
		&ssgenTxIn0,
		&ssgenTxIn1,
	},
	TxOut: []*wire.TxOut{
		&ssgenTxOut0, // [0] block reference
		&ssgenTxOut1, // [1] vote bits
		&ssgenTxOut2, // [2] reward 1
		&ssgenTxOut3, // [3] reward 2
		// Missing consolidation address!
	},
	LockTime: 0,
	Expiry:   0,
}

// ssgenTxOutInvalidConsolidationMarker is a consolidation output with invalid marker.
var ssgenTxOutInvalidConsolidationMarker = wire.TxOut{
	Value:    0,
	CoinType: cointype.CoinTypeVAR,
	Version:  0,
	PkScript: []byte{
		0x6a,       // OP_RETURN
		0x16,       // OP_DATA_22
		0x99, 0x99, // Invalid marker (not "SC")
		0x1a, 0x2b, 0x3c, 0x4d, // hash160 (20 bytes)
		0x5e, 0x6f, 0x7a, 0x8b,
		0x9c, 0x0d, 0x1e, 0x2f,
		0x3a, 0x4b, 0x5c, 0x6d,
		0x7e, 0x8f, 0x9a, 0x0b,
	},
}

// ssgenMsgTxInvalidConsolidationMarker has consolidation with wrong marker.
var ssgenMsgTxInvalidConsolidationMarker = &wire.MsgTx{
	SerType: wire.TxSerializeFull,
	Version: 1,
	TxIn: []*wire.TxIn{
		&ssgenTxIn0,
		&ssgenTxIn1,
	},
	TxOut: []*wire.TxOut{
		&ssgenTxOut0,
		&ssgenTxOut1,
		&ssgenTxOut2,
		&ssgenTxOut3,
		&ssgenTxOutInvalidConsolidationMarker, // Invalid marker
	},
	LockTime: 0,
	Expiry:   0,
}

// ssgenTxOutInvalidConsolidationLength is a consolidation output with wrong length.
var ssgenTxOutInvalidConsolidationLength = wire.TxOut{
	Value:    0,
	CoinType: cointype.CoinTypeVAR,
	Version:  0,
	PkScript: []byte{
		0x6a,       // OP_RETURN
		0x15,       // OP_DATA_21 (wrong, should be 22)
		0x53, 0x43, // "SC"
		0x1a, 0x2b, 0x3c, 0x4d, // Only 19 bytes (should be 20)
		0x5e, 0x6f, 0x7a, 0x8b,
		0x9c, 0x0d, 0x1e, 0x2f,
		0x3a, 0x4b, 0x5c, 0x6d,
		0x7e, 0x8f, 0x9a,
	},
}

// ssgenMsgTxInvalidConsolidationLength has consolidation with wrong length.
var ssgenMsgTxInvalidConsolidationLength = &wire.MsgTx{
	SerType: wire.TxSerializeFull,
	Version: 1,
	TxIn: []*wire.TxIn{
		&ssgenTxIn0,
		&ssgenTxIn1,
	},
	TxOut: []*wire.TxOut{
		&ssgenTxOut0,
		&ssgenTxOut1,
		&ssgenTxOut2,
		&ssgenTxOut3,
		&ssgenTxOutInvalidConsolidationLength, // Wrong length
	},
	LockTime: 0,
	Expiry:   0,
}

// TestCheckSSGenWithConsolidation ensures CheckSSGen correctly validates
// vote transactions with consolidation address outputs.
func TestCheckSSGenWithConsolidation(t *testing.T) {
	tests := []struct {
		name string
		tx   *wire.MsgTx
	}{
		{
			name: "valid vote with consolidation at position 4",
			tx:   ssgenMsgTxWithConsolidation,
		},
		{
			name: "valid vote with consolidation at position 2",
			tx:   ssgenMsgTxWithConsolidationAtPos2,
		},
		{
			name: "valid vote with consolidation and treasury vote",
			tx:   ssgenMsgTxWithConsolidationAndTreasury,
		},
		{
			name: "valid vote with P2SH consolidation",
			tx:   ssgenMsgTxWithScriptHashConsolidation,
		},
		{
			name: "valid vote with P2SH consolidation and treasury vote",
			tx:   ssgenMsgTxWithScriptHashConsolidationAndTreasury,
		},
		{
			name: "valid vote with P2SH and P2PKH consolidation",
			tx:   ssgenMsgTxWithBothConsolidations,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create dcrutil.Tx wrapper
			tx := dcrutil.NewTx(tt.tx)
			tx.SetTree(wire.TxTreeStake)
			tx.SetIndex(0)

			// Test CheckSSGen
			err := CheckSSGen(tx.MsgTx())
			if err != nil {
				t.Errorf("CheckSSGen returned unexpected error: %v", err)
			}

			// Test IsSSGen
			if !IsSSGen(tx.MsgTx()) {
				t.Errorf("IsSSGen claimed a valid SSGen with consolidation is invalid")
			}

			// Test CheckSSGenVotes
			votes, err := CheckSSGenVotes(tx.MsgTx())
			if err != nil {
				t.Errorf("CheckSSGenVotes returned unexpected error: %v", err)
			}

			// For treasury vote test, verify votes were extracted
			if tt.name == "valid vote with consolidation and treasury vote" {
				if len(votes) == 0 {
					t.Errorf("Expected treasury votes to be extracted, got none")
				}
			}
		})
	}
}

// TestCheckSSGenMissingConsolidation ensures CheckSSGen correctly rejects
// vote transactions without consolidation address outputs.
func TestCheckSSGenMissingConsolidation(t *testing.T) {
	tests := []struct {
		name      string
		tx        *wire.MsgTx
		wantErr   ErrorKind
		errSubstr string
	}{
		{
			name:      "vote without consolidation address",
			tx:        ssgenMsgTxNoConsolidation,
			wantErr:   ErrSSGenMissingConsolidation,
			errSubstr: "consolidation address",
		},
		{
			name:      "vote with invalid consolidation marker",
			tx:        ssgenMsgTxInvalidConsolidationMarker,
			wantErr:   ErrSSGenMissingConsolidation,
			errSubstr: "consolidation address",
		},
		{
			name:      "vote with invalid consolidation length",
			tx:        ssgenMsgTxInvalidConsolidationLength,
			wantErr:   ErrSSGenMissingConsolidation,
			errSubstr: "consolidation address",
		},
		{
			name:      "vote with SH consolidation with wrong push opcode",
			tx:        ssgenMsgTxWithMalformedScriptHash(false, shWrongPush),
			wantErr:   ErrSSGenMissingConsolidation,
			errSubstr: "consolidation address",
		},
		{
			name:      "vote with SH consolidation without OP_RETURN",
			tx:        ssgenMsgTxWithMalformedScriptHash(false, shNoOpReturn),
			wantErr:   ErrSSGenMissingConsolidation,
			errSubstr: "consolidation address",
		},
		{
			name:      "vote with SH consolidation of wrong length",
			tx:        ssgenMsgTxWithMalformedScriptHash(false, shWrongLength),
			wantErr:   ErrSSGenMissingConsolidation,
			errSubstr: "consolidation address",
		},
		{
			name:      "vote with SH consolidation one byte too long",
			tx:        ssgenMsgTxWithMalformedScriptHash(false, shTooLong),
			wantErr:   ErrSSGenMissingConsolidation,
			errSubstr: "consolidation address",
		},
		{
			name:      "vote with H marker and wrong first marker byte",
			tx:        ssgenMsgTxWithMalformedScriptHash(false, shWrongFirstMarker),
			wantErr:   ErrSSGenMissingConsolidation,
			errSubstr: "consolidation address",
		},
		{
			name:      "vote with S marker and wrong second marker byte",
			tx:        ssgenMsgTxWithMalformedScriptHash(false, shWrongSecondMarker),
			wantErr:   ErrSSGenMissingConsolidation,
			errSubstr: "consolidation address",
		},
		{
			name:      "vote with SC consolidation and SH output with wrong push opcode",
			tx:        ssgenMsgTxWithMalformedScriptHash(true, shWrongPush),
			wantErr:   ErrSSGenBadGenOuts,
			errSubstr: "OP_SSGEN tagged",
		},
		{
			name:      "vote with SC consolidation and SH output without OP_RETURN",
			tx:        ssgenMsgTxWithMalformedScriptHash(true, shNoOpReturn),
			wantErr:   ErrSSGenBadGenOuts,
			errSubstr: "OP_SSGEN tagged",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create dcrutil.Tx wrapper
			tx := dcrutil.NewTx(tt.tx)
			tx.SetTree(wire.TxTreeStake)
			tx.SetIndex(0)

			// Test CheckSSGen
			err := CheckSSGen(tx.MsgTx())
			if err == nil {
				t.Fatalf("CheckSSGen should have returned error for %s", tt.name)
			}

			if !errors.Is(err, tt.wantErr) {
				t.Errorf("CheckSSGen returned wrong error\nwant: %v\ngot:  %v",
					tt.wantErr, err)
			}

			// Verify error message contains expected substring
			if tt.errSubstr != "" && err.Error() != "" {
				errStr := err.Error()
				found := false
				// Simple substring check
				for i := 0; i <= len(errStr)-len(tt.errSubstr); i++ {
					if errStr[i:i+len(tt.errSubstr)] == tt.errSubstr {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Error message doesn't contain expected substring\nwant substring: %q\ngot: %q",
						tt.errSubstr, errStr)
				}
			}

			// Test IsSSGen
			if IsSSGen(tx.MsgTx()) {
				t.Errorf("IsSSGen claimed an invalid SSGen (missing consolidation) is valid")
			}

			// Test CheckSSGenVotes
			_, err = CheckSSGenVotes(tx.MsgTx())
			if err == nil {
				t.Fatalf("CheckSSGenVotes should have returned error for %s", tt.name)
			}

			if !errors.Is(err, tt.wantErr) {
				t.Errorf("CheckSSGenVotes returned wrong error\nwant: %v\ngot:  %v",
					tt.wantErr, err)
			}
		})
	}
}

// TestIsSSGenWithConsolidation verifies IsSSGen helper function works correctly
// with consolidation address validation.
func TestIsSSGenWithConsolidation(t *testing.T) {
	// Valid votes should return true
	validVotes := []*wire.MsgTx{
		ssgenMsgTxWithConsolidation,
		ssgenMsgTxWithConsolidationAtPos2,
		ssgenMsgTxWithConsolidationAndTreasury,
	}

	for i, tx := range validVotes {
		if !IsSSGen(tx) {
			t.Errorf("IsSSGen returned false for valid vote #%d", i)
		}
	}

	// Invalid votes should return false
	invalidVotes := []*wire.MsgTx{
		ssgenMsgTxNoConsolidation,
		ssgenMsgTxInvalidConsolidationMarker,
		ssgenMsgTxInvalidConsolidationLength,
		ssgenMsgTxWithMalformedScriptHash(false, shWrongSecondMarker),
	}

	for i, tx := range invalidVotes {
		if IsSSGen(tx) {
			t.Errorf("IsSSGen returned true for invalid vote #%d", i)
		}
	}
}

// TestConsolidationOutputSkippedInRewardValidation verifies that the
// consolidation address output is properly skipped when validating reward
// outputs (which must be OP_SSGEN tagged).
func TestConsolidationOutputSkippedInRewardValidation(t *testing.T) {
	// Create a vote with consolidation in the middle of rewards
	// This tests that the skip logic works correctly
	tx := &wire.MsgTx{
		SerType: wire.TxSerializeFull,
		Version: 1,
		TxIn: []*wire.TxIn{
			&ssgenTxIn0,
			&ssgenTxIn1,
		},
		TxOut: []*wire.TxOut{
			&ssgenTxOut0,               // [0] block reference
			&ssgenTxOut1,               // [1] vote bits
			&ssgenTxOut2,               // [2] reward 1 (OP_SSGEN)
			buildConsolidationOutput(), // [3] consolidation (OP_RETURN)
			&ssgenTxOut3,               // [4] reward 2 (OP_SSGEN)
		},
		LockTime: 0,
		Expiry:   0,
	}

	err := CheckSSGen(tx)
	if err != nil {
		t.Errorf("CheckSSGen failed with consolidation in middle of rewards: %v", err)
	}

	if !IsSSGen(tx) {
		t.Errorf("IsSSGen returned false for valid vote with consolidation in middle")
	}
}
