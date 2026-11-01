// Copyright (c) 2024 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package stake

import (
	"encoding/binary"
	"fmt"

	"github.com/monetarium/monetarium-node/blockchain/standalone"
	"github.com/monetarium/monetarium-node/wire"
)

// SSFee marker detection and validation utilities.
// These functions provide a centralized, consistent way to detect and create
// SSFee OP_RETURN markers across the codebase.
//
// This package imports constants from blockchain/standalone to avoid duplication.

// SSFeeMarkerType represents the type of SSFee transaction.
type SSFeeMarkerType int

const (
	// SSFeeMarkerNone indicates the script does not contain an SSFee marker.
	SSFeeMarkerNone SSFeeMarkerType = iota

	// SSFeeMarkerStaker indicates a staker fee marker ("SF").
	SSFeeMarkerStaker

	// SSFeeMarkerMiner indicates a miner fee marker ("MF").
	SSFeeMarkerMiner
)

// HasSSFeeMarker checks if a script contains a valid SSFee OP_RETURN marker.
// Returns the marker type (Staker/Miner) or None if not found.
//
// SSFee OP_RETURN format:
//   - Staker: OP_RETURN + OP_DATA_8 + "SF" + height(4 bytes) + voter_seq(2 bytes)
//   - Miner:  OP_RETURN + OP_DATA_6 + "MF" + height(4 bytes)
func HasSSFeeMarker(script []byte) SSFeeMarkerType {
	// Check minimum length: OP_RETURN + OP_DATA_6/8 + "SF"/"MF" + height(4)
	if len(script) < standalone.SSFeeMinScriptLen {
		return SSFeeMarkerNone
	}

	// Check for OP_RETURN
	if script[0] != standalone.SSFeeOpReturn {
		return SSFeeMarkerNone
	}

	// Check for OP_DATA_6 (miner) or OP_DATA_8 (staker)
	if script[1] != standalone.SSFeeOpData6 && script[1] != standalone.SSFeeOpData8 {
		return SSFeeMarkerNone
	}

	// Check for "SF" (Stake Fee) marker
	if script[2] == standalone.SSFeeMarkerS && script[3] == standalone.SSFeeMarkerF {
		return SSFeeMarkerStaker
	}

	// Check for "MF" (Miner Fee) marker
	if script[2] == standalone.SSFeeMarkerM && script[3] == standalone.SSFeeMarkerF {
		return SSFeeMarkerMiner
	}

	return SSFeeMarkerNone
}

// IsSSFeeMarkerScript checks if a script is a valid SSFee OP_RETURN.
// This is a convenience function that returns a boolean instead of the marker type.
func IsSSFeeMarkerScript(script []byte) bool {
	return HasSSFeeMarker(script) != SSFeeMarkerNone
}

// CreateStakerSSFeeMarker creates an OP_RETURN script for staker SSFee transactions.
//
// Format: OP_RETURN + OP_DATA_8 + "SF" + height(4 bytes) + voter_seq(2 bytes)
//
// The voter sequence distinguishes multiple SSFee transactions in the same block,
// ensuring each has a unique hash.
func CreateStakerSSFeeMarker(blockHeight int64, voterSeq uint16) []byte {
	script := make([]byte, 10) // 1 + 1 + 2 + 4 + 2
	script[0] = standalone.SSFeeOpReturn
	script[1] = standalone.SSFeeOpData8
	script[2] = standalone.SSFeeMarkerS // 'S'
	script[3] = standalone.SSFeeMarkerF // 'F'
	binary.LittleEndian.PutUint32(script[4:8], uint32(blockHeight))
	binary.LittleEndian.PutUint16(script[8:10], voterSeq)
	return script
}

// CreateMinerSSFeeMarker creates an OP_RETURN script for miner SSFee transactions.
//
// Format: OP_RETURN + OP_DATA_6 + "MF" + height(4 bytes)
//
// Miner SSFee transactions don't need a voter sequence since there's only one
// miner per block per coin type.
func CreateMinerSSFeeMarker(blockHeight int64) []byte {
	script := make([]byte, 8) // 1 + 1 + 2 + 4
	script[0] = standalone.SSFeeOpReturn
	script[1] = standalone.SSFeeOpData6
	script[2] = standalone.SSFeeMarkerM // 'M'
	script[3] = standalone.SSFeeMarkerF // 'F'
	binary.LittleEndian.PutUint32(script[4:8], uint32(blockHeight))
	return script
}

// -----------------------------------------------------------------------------
// Consolidation Address Utilities
//
// These functions handle the consolidation address in vote (SSGen) transactions.
// The consolidation address specifies where SSFee payments should be sent,
// enabling UTXO augmentation to reduce dust UTXOs.
// -----------------------------------------------------------------------------

const (
	// SSConsolidationOutputSize is the size of the consolidation address
	// OP_RETURN output in vote transactions.
	// Format: OP_RETURN + OP_DATA_22 + "SC"/"SH" + hash160(20 bytes) = 24 bytes
	SSConsolidationOutputSize = 24

	// SSConsolidationMarkerS is the first byte of the "SC" and "SH" markers.
	SSConsolidationMarkerS = 0x53 // 'S'

	// SSConsolidationMarkerC is the second byte of the "SC" marker, which
	// makes the hash160 a P2PKH address.
	SSConsolidationMarkerC = 0x43 // 'C'

	// SSConsolidationMarkerH is the second byte of the "SH" marker, which
	// makes the hash160 a P2SH address.
	SSConsolidationMarkerH = 0x48 // 'H'

	// SSConsolidationOpData22 is the OP_DATA push size for consolidation output.
	SSConsolidationOpData22 = 0x16 // 22 bytes
)

// parseConsolidationOutput returns the hash160 of a consolidation address
// output and whether it is a P2SH address.  The hash160 is nil when the script
// is not a consolidation address output.
func parseConsolidationOutput(script []byte) ([]byte, bool) {
	if len(script) != SSConsolidationOutputSize ||
		script[0] != standalone.SSFeeOpReturn ||
		script[1] != SSConsolidationOpData22 ||
		script[2] != SSConsolidationMarkerS {
		return nil, false
	}
	switch script[3] {
	case SSConsolidationMarkerC:
		return script[4:24], false
	case SSConsolidationMarkerH:
		return script[4:24], true
	}
	return nil, false
}

// ExtractSSFeeConsolidationScript returns the script that staker SSFee
// payments to the voter of a vote (SSGen) transaction must pay: the
// OP_SSGEN-tagged P2PKH script of an "SC" consolidation address, or the
// OP_SSGEN-tagged P2SH script of an "SH" one.
//
// Vote transaction outputs:
//
//	[0] Block reference (OP_RETURN + 40 bytes)
//	[1] Vote bits (OP_RETURN + votebits)
//	[2..N] VAR reward outputs (P2PKH/P2SH)
//	[N+1] Consolidation address (OP_RETURN + "SC"/"SH" + hash160)  ← This function reads this
//	[N+2] Treasury vote (OP_RETURN + "TV" + ...) [optional, always last]
//
// The function scans outputs from index 2 onwards and uses the first
// consolidation address output.  It stops before the last output if it's a
// treasury vote.
//
// Returns an error if no consolidation address output is found.
func ExtractSSFeeConsolidationScript(voteTx *wire.MsgTx) ([]byte, error) {
	// Need at least 3 outputs: block ref [0], vote bits [1], and consolidation [2+]
	if len(voteTx.TxOut) < 3 {
		return nil, fmt.Errorf("vote transaction has too few outputs (%d, need at least 3)",
			len(voteTx.TxOut))
	}

	// Determine scan range: skip outputs [0] and [1], stop before treasury vote if present
	endIdx := len(voteTx.TxOut)
	lastOutput := voteTx.TxOut[len(voteTx.TxOut)-1]

	// Check if last output is a treasury vote (OP_RETURN with "TV" marker)
	// Treasury vote format: OP_RETURN + OP_DATA_N + 'T' + 'V' + ...
	if len(lastOutput.PkScript) >= 4 &&
		lastOutput.PkScript[0] == standalone.SSFeeOpReturn &&
		lastOutput.PkScript[2] == 'T' &&
		lastOutput.PkScript[3] == 'V' {
		// Treasury vote is present, don't scan it
		endIdx--
	}

	// Scan outputs from index 2 to endIdx-1 for consolidation marker
	for i := 2; i < endIdx; i++ {
		hash160, isScriptHash := parseConsolidationOutput(voteTx.TxOut[i].PkScript)
		if hash160 == nil {
			continue
		}
		if isScriptHash {
			return ConsolidationScriptHashToPkScript(hash160)
		}
		return ConsolidationAddrToPkScript(hash160)
	}

	return nil, fmt.Errorf("vote transaction missing consolidation address output")
}

// ExtractSSFeeConsolidationAddr returns the hash160 of the consolidation
// address of a vote (SSGen) transaction, without its type: a P2SH ("SH")
// address returns its hash160 like a P2PKH ("SC") one.  Callers that pay the
// address must use ExtractSSFeeConsolidationScript, which keeps the type.
//
// It does not reject "SH", so that a block template generator built against
// it does not fail on every template that includes such a vote.  Paying the
// hash160 as P2PKH sends that voter's share to a script no key can satisfy,
// as before "SH" existed, and validation that reads
// ExtractSSFeeConsolidationScript rejects such a payout.
func ExtractSSFeeConsolidationAddr(voteTx *wire.MsgTx) ([]byte, error) {
	script, err := ExtractSSFeeConsolidationScript(voteTx)
	if err != nil {
		return nil, err
	}
	if isPubKeyHashScript(script[1:]) {
		return script[4:24], nil
	}
	return script[3:23], nil
}

// CreateSSFeeConsolidationOutput creates an OP_RETURN output for vote construction
// that specifies the P2PKH address SSFee payments should be sent to.
//
// Format: OP_RETURN + OP_DATA_22 + "SC" + hash160(20 bytes) = 24 bytes
//
// Example:
//
//	hash160 := []byte{0x1a, 0x2b, ..., 0x0b} // 20 bytes
//	output := CreateSSFeeConsolidationOutput(hash160)
//	// output.PkScript = [0x6a, 0x16, 0x53, 0x43, 0x1a, 0x2b, ..., 0x0b]
//
// The returned TxOut has Value=0 and should be inserted into the vote transaction
// after all reward outputs but before any treasury vote output.
func CreateSSFeeConsolidationOutput(hash160 []byte) (*wire.TxOut, error) {
	return createConsolidationOutput(SSConsolidationMarkerC, hash160)
}

// CreateSSFeeScriptHashConsolidationOutput is CreateSSFeeConsolidationOutput
// for a P2SH address: the output carries the "SH" marker instead of "SC".
func CreateSSFeeScriptHashConsolidationOutput(hash160 []byte) (*wire.TxOut, error) {
	return createConsolidationOutput(SSConsolidationMarkerH, hash160)
}

// createConsolidationOutput returns a consolidation address output for hash160
// whose second marker byte is marker, which must be SSConsolidationMarkerC or
// SSConsolidationMarkerH.
func createConsolidationOutput(marker byte, hash160 []byte) (*wire.TxOut, error) {
	if len(hash160) != 20 {
		return nil, fmt.Errorf("invalid hash160 length: %d (expected 20)", len(hash160))
	}

	script := make([]byte, SSConsolidationOutputSize)
	script[0] = standalone.SSFeeOpReturn // OP_RETURN
	script[1] = SSConsolidationOpData22  // OP_DATA_22
	script[2] = SSConsolidationMarkerS   // 'S'
	script[3] = marker                   // 'C' or 'H'
	copy(script[4:24], hash160)          // hash160 (20 bytes)

	return &wire.TxOut{
		Value:    0,
		Version:  0,
		PkScript: script,
	}, nil
}

// ConsolidationAddrToPkScript converts a consolidation address hash160 to an
// OP_SSGEN-tagged P2PKH pkScript for SSFee outputs.
//
// SSFee outputs must use OP_SSGEN-tagged scripts so that the wallet correctly
// identifies them as stake tree outputs. Without the OP_SSGEN prefix, the wallet
// would use TxTreeRegular when creating spending transactions, but mond stores
// SSFee UTXOs with TxTreeStake (since they're in the stake tree). This mismatch
// causes UTXO lookup failures.
//
// This is the inverse of extractHash160FromPkScript in the indexers package,
// but with the OP_SSGEN prefix prepended.
//
// Format: OP_SSGEN OP_DUP OP_HASH160 OP_DATA_20 <hash160> OP_EQUALVERIFY OP_CHECKSIG = 26 bytes
//
// Example:
//
//	hash160 := []byte{0x1a, 0x2b, ..., 0x0b} // 20 bytes
//	pkScript := ConsolidationAddrToPkScript(hash160)
//	// pkScript = [0xbb, 0x76, 0xa9, 0x14, 0x1a, 0x2b, ..., 0x0b, 0x88, 0xac]
func ConsolidationAddrToPkScript(hash160 []byte) ([]byte, error) {
	if len(hash160) != 20 {
		return nil, fmt.Errorf("invalid hash160 length: %d (expected 20)", len(hash160))
	}

	// OP_SSGEN-tagged P2PKH script for stake tree outputs:
	// OP_SSGEN OP_DUP OP_HASH160 OP_DATA_20 <hash160> OP_EQUALVERIFY OP_CHECKSIG
	const opSSGen = 0xbb // txscript.OP_SSGEN
	script := make([]byte, 26)
	script[0] = opSSGen // OP_SSGEN
	script[1] = 0x76    // OP_DUP
	script[2] = 0xa9    // OP_HASH160
	script[3] = 0x14    // OP_DATA_20
	copy(script[4:24], hash160)
	script[24] = 0x88 // OP_EQUALVERIFY
	script[25] = 0xac // OP_CHECKSIG

	return script, nil
}

// ConsolidationScriptHashToPkScript is ConsolidationAddrToPkScript for a P2SH
// consolidation address: it returns the OP_SSGEN-tagged P2SH pkScript.
//
// Format: OP_SSGEN OP_HASH160 OP_DATA_20 <hash160> OP_EQUAL = 24 bytes
func ConsolidationScriptHashToPkScript(hash160 []byte) ([]byte, error) {
	if len(hash160) != 20 {
		return nil, fmt.Errorf("invalid hash160 length: %d (expected 20)", len(hash160))
	}

	const opSSGen = 0xbb // txscript.OP_SSGEN
	script := make([]byte, 24)
	script[0] = opSSGen // OP_SSGEN
	script[1] = 0xa9    // OP_HASH160
	script[2] = 0x14    // OP_DATA_20
	copy(script[3:23], hash160)
	script[23] = 0x87 // OP_EQUAL

	return script, nil
}
