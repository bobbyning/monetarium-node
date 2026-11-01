// Copyright (c) 2024 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package indexers

import (
	"context"
	"fmt"
	"math/big"
	"sync"

	"github.com/monetarium/monetarium-node/blockchain/stake"
	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/chaincfg/chainhash"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/database"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/txscript"
	"github.com/monetarium/monetarium-node/wire"
)

const (
	// ssfeeIndexName is the human-readable name for the index.
	ssfeeIndexName = "ssfee utxo index"

	// ssfeeIndexVersion is the current version of the SSFee UTXO index.
	// Version 2: Added feeType (SF/MF) to key to separate staker and miner UTXOs
	ssfeeIndexVersion = 2

	// ssfeeKeyPrefix is the prefix used for all SSFee index keys.
	ssfeeKeyPrefix = "sf"

	// ssfeeKeySize is the total size of an SSFee index key.
	// Format: prefix(2) + feeType(1) + coinType(1) + addressHash160(20) = 24 bytes
	// feeType: 0=SF (staker), 1=MF (miner)
	ssfeeKeySize = 24

	// SSFee type constants for index key
	ssfeeTypeStaker byte = 0 // SF - staker fee
	ssfeeTypeMiner  byte = 1 // MF - miner fee

	// outpointSize is the serialized size of a wire.OutPoint.
	// Format: hash(32) + index(4) + tree(1) = 37 bytes
	outpointSize = 37
)

var (
	// ssfeeIndexKey is the key of the SSFee UTXO index and the db bucket
	// used to house it.
	ssfeeIndexKey = []byte("ssfeeindex")
)

// SSFeeIndex implements an index that tracks SSFee (Stake Fee) transaction outputs
// by (coinType, address) for efficient UTXO lookup during block template generation.
//
// This index enables UTXO augmentation, where SSFee transactions can reuse existing
// UTXOs as inputs instead of creating new dust UTXOs for each fee distribution.
//
// Index Structure:
//
//	Key: "sf" + coinType(1 byte) + addressHash160(20 bytes)
//	Value: Serialized list of OutPoint structs pointing to unspent SSFee outputs
//
// The index is updated as blocks are connected and disconnected from the main chain.
type SSFeeIndex struct {
	// The following fields are set when the instance is created and can't
	// be changed afterwards, so there is no need to protect them with a
	// separate mutex.
	db    database.DB
	chain ChainQueryer
	sub   *IndexSubscription

	// subscribers is a map of clients that are waiting for the index to
	// signal it has completed syncing.
	subscribers map[chan bool]struct{}

	// mtx protects concurrent access to the subscribers map.
	mtx sync.Mutex

	// cancel enables the caller to cancel long running operations.
	cancel context.CancelFunc
}

// Ensure SSFeeIndex implements the Indexer interface.
var _ Indexer = (*SSFeeIndex)(nil)

// NewSSFeeIndex returns a new instance of an indexer that tracks SSFee outputs
// by (coinType, address) for efficient UTXO lookup.
func NewSSFeeIndex(subscriber *IndexSubscriber, db database.DB, chain ChainQueryer) (*SSFeeIndex, error) {
	idx := &SSFeeIndex{
		db:          db,
		chain:       chain,
		subscribers: make(map[chan bool]struct{}),
		cancel:      subscriber.cancel,
	}
	sub, err := subscriber.Subscribe(idx, noPrereqs)
	if err != nil {
		return nil, err
	}
	idx.sub = sub
	err = idx.Init(subscriber.ctx, chain.ChainParams())
	if err != nil {
		return nil, err
	}
	return idx, nil
}

// Key returns the key of the index as a byte slice.
//
// This is part of the Indexer interface.
func (idx *SSFeeIndex) Key() []byte {
	return ssfeeIndexKey
}

// Name returns the human-readable name of the index.
//
// This is part of the Indexer interface.
func (idx *SSFeeIndex) Name() string {
	return ssfeeIndexName
}

// Version returns the current version of the index.
//
// This is part of the Indexer interface.
func (idx *SSFeeIndex) Version() uint32 {
	return ssfeeIndexVersion
}

// DB returns the database of the index.
//
// This is part of the Indexer interface.
func (idx *SSFeeIndex) DB() database.DB {
	return idx.db
}

// Queryer returns the chain queryer.
//
// This is part of the Indexer interface.
func (idx *SSFeeIndex) Queryer() ChainQueryer {
	return idx.chain
}

// Tip returns the current tip of the index.
//
// This is part of the Indexer interface.
func (idx *SSFeeIndex) Tip() (int64, *chainhash.Hash, error) {
	var height int64
	var hash *chainhash.Hash
	err := idx.db.View(func(dbTx database.Tx) error {
		h, height32, err := dbFetchIndexerTip(dbTx, ssfeeIndexKey)
		if err != nil {
			return err
		}
		hash = h
		height = int64(height32)
		return nil
	})
	return height, hash, err
}

// Create is invoked when the indexer is being created.
//
// This is part of the Indexer interface.
func (idx *SSFeeIndex) Create(dbTx database.Tx) error {
	// Create the bucket that houses the index.
	_, err := dbTx.Metadata().CreateBucketIfNotExists(ssfeeIndexKey)
	return err
}

// Init is invoked when the index is being initialized.
// This differs from the Create method in that it is called on
// every load, including the case the index was just created.
//
// This is part of the Indexer interface.
func (idx *SSFeeIndex) Init(ctx context.Context, chainParams *chaincfg.Params) error {
	if interruptRequested(ctx) {
		return indexerError(ErrInterruptRequested, interruptMsg)
	}

	// Create the initial state for the index as needed.
	if err := createIndex(idx, &chainParams.GenesisHash); err != nil {
		return err
	}

	// Recover the ssfee index to the main chain if needed.
	return recoverIndex(ctx, idx)
}

// IndexSubscription returns the subscription for the index.
//
// This is part of the Indexer interface.
func (idx *SSFeeIndex) IndexSubscription() *IndexSubscription {
	return idx.sub
}

// WaitForSync subscribes clients for the next index sync update.
//
// This is part of the Indexer interface.
func (idx *SSFeeIndex) WaitForSync() chan bool {
	c := make(chan bool)
	idx.mtx.Lock()
	idx.subscribers[c] = struct{}{}
	idx.mtx.Unlock()
	return c
}

// NotifySyncSubscribers notifies all subscribers that the index has
// completed syncing.
//
// This is part of the Indexer interface.
func (idx *SSFeeIndex) NotifySyncSubscribers() {
	idx.mtx.Lock()
	notifySyncSubscribers(idx.subscribers)
	idx.mtx.Unlock()
}

// ProcessNotification indexes the provided notification based on its
// type.  This allows the index to stay synchronized with the chain.
//
// This is part of the Indexer interface.
func (idx *SSFeeIndex) ProcessNotification(dbTx database.Tx, ntfn *IndexNtfn) error {
	switch ntfn.NtfnType {
	case ConnectNtfn:
		if err := idx.ConnectBlock(dbTx, ntfn.Block); err != nil {
			return err
		}

	case DisconnectNtfn:
		if err := idx.DisconnectBlock(dbTx, ntfn.Block); err != nil {
			return err
		}
	}
	return nil
}

// makeSSFeeIndexKey creates an index key for the given fee type, coinType and address hash160.
//
// Format: "sf" + feeType(1 byte) + coinType(1 byte) + addressHash160(20 bytes) = 24 bytes
// feeType: 0=SF (staker), 1=MF (miner)
func makeSSFeeIndexKey(feeType byte, coinType cointype.CoinType, hash160 []byte) ([]byte, error) {
	if len(hash160) != 20 {
		return nil, fmt.Errorf("invalid hash160 length: %d (expected 20)", len(hash160))
	}

	key := make([]byte, ssfeeKeySize)
	copy(key[0:2], []byte(ssfeeKeyPrefix))
	key[2] = feeType
	key[3] = byte(coinType)
	copy(key[4:24], hash160)
	return key, nil
}

// extractFeeTypeFromSSFee extracts the fee type (SF/MF) from an SSFee transaction.
// SSFee transactions have an OP_RETURN marker in output[0] containing "SF" or "MF".
// Returns ssfeeTypeStaker (0) for staker fees, ssfeeTypeMiner (1) for miner fees.
func extractFeeTypeFromSSFee(tx *wire.MsgTx) byte {
	if len(tx.TxOut) == 0 {
		return ssfeeTypeStaker // default
	}

	// Check output[0] for OP_RETURN marker
	script := tx.TxOut[0].PkScript
	if len(script) < 4 || script[0] != txscript.OP_RETURN {
		return ssfeeTypeStaker // default
	}

	// Parse the OP_RETURN data to find SF/MF marker
	// Format: OP_RETURN OP_DATA_N <data>
	if script[1] >= 1 && script[1] <= 75 {
		dataLen := int(script[1])
		if len(script) >= 2+dataLen {
			data := script[2 : 2+dataLen]
			// Look for "MF" in the data
			for i := 0; i < len(data)-1; i++ {
				if data[i] == 'M' && data[i+1] == 'F' {
					return ssfeeTypeMiner
				}
			}
		}
	}

	return ssfeeTypeStaker
}

// extractHash160FromPkScript extracts the address hash160 from a P2PKH script.
//
// Supports two formats:
//
//  1. Standard P2PKH (25 bytes):
//     OP_DUP OP_HASH160 <20 bytes hash160> OP_EQUALVERIFY OP_CHECKSIG
//
//  2. OP_SSGEN-tagged P2PKH (26 bytes) - used by SSFee outputs:
//     OP_SSGEN OP_DUP OP_HASH160 <20 bytes hash160> OP_EQUALVERIFY OP_CHECKSIG
func extractHash160FromPkScript(pkScript []byte) ([]byte, error) {
	const opSSGen = 0xbb // txscript.OP_SSGEN

	// Check for OP_SSGEN-tagged P2PKH (26 bytes)
	if len(pkScript) == 26 {
		// Validate OP_SSGEN prefix
		if pkScript[0] != opSSGen {
			return nil, fmt.Errorf("26-byte script must start with OP_SSGEN")
		}

		// Validate P2PKH structure after OP_SSGEN
		if pkScript[1] != txscript.OP_DUP ||
			pkScript[2] != txscript.OP_HASH160 ||
			pkScript[3] != txscript.OP_DATA_20 ||
			pkScript[24] != txscript.OP_EQUALVERIFY ||
			pkScript[25] != txscript.OP_CHECKSIG {
			return nil, fmt.Errorf("not a valid OP_SSGEN-tagged P2PKH script")
		}

		// Extract hash160 (bytes 4-23)
		hash160 := make([]byte, 20)
		copy(hash160, pkScript[4:24])
		return hash160, nil
	}

	// Check for standard P2PKH (25 bytes)
	if len(pkScript) == 25 {
		// Validate P2PKH script format
		if pkScript[0] != txscript.OP_DUP ||
			pkScript[1] != txscript.OP_HASH160 ||
			pkScript[2] != txscript.OP_DATA_20 ||
			pkScript[23] != txscript.OP_EQUALVERIFY ||
			pkScript[24] != txscript.OP_CHECKSIG {
			return nil, fmt.Errorf("not a valid P2PKH script")
		}

		// Extract hash160 (bytes 3-22)
		hash160 := make([]byte, 20)
		copy(hash160, pkScript[3:23])
		return hash160, nil
	}

	return nil, fmt.Errorf("invalid P2PKH script length: %d (expected 25 or 26)", len(pkScript))
}

// serializeOutPoints serializes a list of OutPoints into a byte slice.
//
// Each OutPoint is serialized as: hash(32) + index(4) + tree(1) = 37 bytes
func serializeOutPoints(outpoints []wire.OutPoint) []byte {
	if len(outpoints) == 0 {
		return []byte{}
	}

	buf := make([]byte, len(outpoints)*outpointSize)
	offset := 0

	for _, op := range outpoints {
		// Serialize hash (32 bytes)
		copy(buf[offset:offset+32], op.Hash[:])

		// Serialize index (4 bytes, little-endian)
		byteOrder.PutUint32(buf[offset+32:offset+36], op.Index)

		// Serialize tree (1 byte)
		buf[offset+36] = byte(op.Tree)

		offset += outpointSize
	}

	return buf
}

// deserializeOutPoints deserializes a byte slice into a list of OutPoints.
//
// Each OutPoint is deserialized from: hash(32) + index(4) + tree(1) = 37 bytes
func deserializeOutPoints(data []byte) ([]wire.OutPoint, error) {
	if len(data) == 0 {
		return []wire.OutPoint{}, nil
	}

	if len(data)%outpointSize != 0 {
		return nil, fmt.Errorf("invalid outpoint data length: %d (must be multiple of %d)",
			len(data), outpointSize)
	}

	numOutpoints := len(data) / outpointSize
	outpoints := make([]wire.OutPoint, numOutpoints)
	offset := 0

	for i := 0; i < numOutpoints; i++ {
		// Deserialize hash (32 bytes)
		var hash chainhash.Hash
		copy(hash[:], data[offset:offset+32])

		// Deserialize index (4 bytes, little-endian)
		index := byteOrder.Uint32(data[offset+32 : offset+36])

		// Deserialize tree (1 byte)
		tree := int8(data[offset+36])

		outpoints[i] = wire.OutPoint{
			Hash:  hash,
			Index: index,
			Tree:  tree,
		}

		offset += outpointSize
	}

	return outpoints, nil
}

// ConnectBlock indexes all SSFee outputs in the provided block.
// This is called when a block is connected to the main chain.
//
// For each SSFee transaction in the block:
//  1. Extract the address hash160 from the payment at output[1]
//  2. Create index key: "sf" + feeType + coinType + addressHash160
//  3. Add the outpoint to the list for this key
//
// This is part of the Indexer interface implementation via ProcessNotification.
func (idx *SSFeeIndex) ConnectBlock(dbTx database.Tx, block *dcrutil.Block) error {
	// Get the SSFee index bucket
	bucket := dbTx.Metadata().Bucket(ssfeeIndexKey)
	if bucket == nil {
		return fmt.Errorf("ssfee index bucket not found")
	}

	ssfeeCount := 0 // Track number of SSFee txs indexed

	// Iterate through stake transactions in the block
	for _, stx := range block.STransactions() {
		// Check if this is an SSFee transaction
		if !stake.IsSSFee(stx.MsgTx()) {
			continue
		}

		ssfeeCount++

		// Skip an SSFee without the payment output at index 1, since there
		// is nothing to index.
		if len(stx.MsgTx().TxOut) < 2 {
			continue
		}

		// SSFee standard format: output[0] = OP_RETURN marker, output[1] = payment
		paymentOutput := stx.MsgTx().TxOut[1]
		const paymentIndex uint32 = 1

		// Extract fee type (SF/MF) from OP_RETURN marker in output[0]
		feeType := extractFeeTypeFromSSFee(stx.MsgTx())

		// Extract hash160 from the payment output
		hash160, err := extractHash160FromPkScript(paymentOutput.PkScript)
		if err != nil {
			// Skip non-P2PKH outputs, such as the P2SH payout to an "SH"
			// consolidation address, which the generator never augments.
			log.Debugf("SSFeeIndex: Skipping SSFee tx %s: failed to extract hash160: %v", stx.Hash(), err)
			continue
		}

		// Create index key for this (feeType, coinType, address)
		key, err := makeSSFeeIndexKey(feeType, paymentOutput.CoinType, hash160)
		if err != nil {
			return fmt.Errorf("failed to create index key: %w", err)
		}

		// Fetch existing outpoints for this key
		existingData := bucket.Get(key)
		existingOutpoints, err := deserializeOutPoints(existingData)
		if err != nil {
			return fmt.Errorf("failed to deserialize existing outpoints: %w", err)
		}

		// If this SSFee tx has a real input (not null), it's augmenting an existing UTXO.
		// We need to remove the spent input's outpoint from the index.
		if len(stx.MsgTx().TxIn) > 0 {
			spentOutpoint := stx.MsgTx().TxIn[0].PreviousOutPoint
			// Check if this is a real input (not a null input like coinbase/treasurybase)
			if spentOutpoint.Index != wire.MaxPrevOutIndex {
				// Remove the spent outpoint from the list
				for i, op := range existingOutpoints {
					if op.Hash.IsEqual(&spentOutpoint.Hash) && op.Index == spentOutpoint.Index && op.Tree == spentOutpoint.Tree {
						// Remove by swapping with last element and truncating
						existingOutpoints[i] = existingOutpoints[len(existingOutpoints)-1]
						existingOutpoints = existingOutpoints[:len(existingOutpoints)-1]
						log.Debugf("SSFeeIndex: Removed spent outpoint %v:%d from index (augmented by %v)",
							spentOutpoint.Hash, spentOutpoint.Index, stx.Hash())
						break
					}
				}
			}
		}

		// Add this outpoint to the list
		newOutpoint := wire.OutPoint{
			Hash:  *stx.Hash(),
			Index: paymentIndex,
			Tree:  wire.TxTreeStake,
		}
		existingOutpoints = append(existingOutpoints, newOutpoint)

		// Serialize and store updated outpoint list
		updatedData := serializeOutPoints(existingOutpoints)
		if err := bucket.Put(key, updatedData); err != nil {
			return fmt.Errorf("failed to store outpoints: %w", err)
		}
	}

	if ssfeeCount > 0 {
		log.Debugf("SSFeeIndex: Indexed %d SSFee transaction(s) in block %s (height %d)",
			ssfeeCount, block.Hash(), block.Height())
	}

	// Update the current index tip.
	return dbPutIndexerTip(dbTx, ssfeeIndexKey, block.Hash(), int32(block.Height()))
}

// DisconnectBlock removes all SSFee outputs from the provided block from the index.
// This is called when a block is disconnected from the main chain (reorg).
//
// For each SSFee transaction in the block:
//  1. Extract the address hash160 from the payment at output[1]
//  2. Create index key: "sf" + feeType + coinType + addressHash160
//  3. Remove the outpoint from the list for this key
//
// This is part of the Indexer interface implementation via ProcessNotification.
func (idx *SSFeeIndex) DisconnectBlock(dbTx database.Tx, block *dcrutil.Block) error {
	// Get the SSFee index bucket
	bucket := dbTx.Metadata().Bucket(ssfeeIndexKey)
	if bucket == nil {
		return fmt.Errorf("ssfee index bucket not found")
	}

	// Iterate through stake transactions in the block
	for _, stx := range block.STransactions() {
		// Check if this is an SSFee transaction
		if !stake.IsSSFee(stx.MsgTx()) {
			continue
		}

		// Skip an SSFee without the payment output at index 1, since there
		// is nothing to index.
		if len(stx.MsgTx().TxOut) < 2 {
			continue
		}

		// SSFee standard format: output[0] = OP_RETURN marker, output[1] = payment
		paymentOutput := stx.MsgTx().TxOut[1]
		const paymentIndex uint32 = 1

		// Extract fee type (SF/MF) from OP_RETURN marker in output[0]
		feeType := extractFeeTypeFromSSFee(stx.MsgTx())

		// Extract hash160 from the payment output
		hash160, err := extractHash160FromPkScript(paymentOutput.PkScript)
		if err != nil {
			// Skip non-P2PKH outputs
			continue
		}

		// Create index key for this (feeType, coinType, address)
		key, err := makeSSFeeIndexKey(feeType, paymentOutput.CoinType, hash160)
		if err != nil {
			return fmt.Errorf("failed to create index key: %w", err)
		}

		// Fetch existing outpoints for this key
		existingData := bucket.Get(key)
		existingOutpoints, err := deserializeOutPoints(existingData)
		if err != nil {
			return fmt.Errorf("failed to deserialize existing outpoints: %w", err)
		}

		// Remove this outpoint from the list
		targetHash := *stx.Hash()
		filtered := make([]wire.OutPoint, 0, len(existingOutpoints))
		for _, op := range existingOutpoints {
			if op.Hash != targetHash || op.Index != paymentIndex {
				filtered = append(filtered, op)
			}
		}

		// If this SSFee tx had a real input (not null), it was augmenting an existing UTXO.
		// Disconnecting means the old input is no longer spent, so add it back to the index.
		if len(stx.MsgTx().TxIn) > 0 {
			spentOutpoint := stx.MsgTx().TxIn[0].PreviousOutPoint
			// Check if this was a real input (not a null input like coinbase/treasurybase)
			if spentOutpoint.Index != wire.MaxPrevOutIndex {
				filtered = append(filtered, spentOutpoint)
				log.Debugf("SSFeeIndex: Re-added outpoint %v:%d to index (disconnect of augmentation %v)",
					spentOutpoint.Hash, spentOutpoint.Index, stx.Hash())
			}
		}

		// If no outpoints remain, delete the key
		if len(filtered) == 0 {
			if err := bucket.Delete(key); err != nil {
				return fmt.Errorf("failed to delete key: %w", err)
			}
		} else {
			// Serialize and store updated outpoint list
			updatedData := serializeOutPoints(filtered)
			if err := bucket.Put(key, updatedData); err != nil {
				return fmt.Errorf("failed to store outpoints: %w", err)
			}
		}
	}

	// Update the current index tip.
	return dbPutIndexerTip(dbTx, ssfeeIndexKey, &block.MsgBlock().Header.PrevBlock,
		int32(block.Height()-1))
}

// LookupUTXO finds an unspent SSFee UTXO for the given (coinType, address).
//
// Returns:
//   - outpoint: The first unspent outpoint found, or nil if none exist
//   - value: The value of the UTXO as *big.Int (supports SKA large values)
//   - blockHeight: The block height where the UTXO was created (for fraud proofs)
//   - blockIndex: The transaction index within the block (for fraud proofs)
//   - error: Any error encountered during lookup
//
// This is the primary query method used by block template generation to find
// existing SSFee UTXOs for augmentation.
//
// The isMiner parameter specifies whether to look up miner (true) or staker (false) UTXOs.
// Miner and staker SSFees are tracked separately to avoid collisions.
func (idx *SSFeeIndex) LookupUTXO(isMiner bool, coinType cointype.CoinType, addressHash160 []byte) (*wire.OutPoint, *big.Int, int64, uint32, error) {
	// Determine fee type based on caller
	feeType := ssfeeTypeStaker
	if isMiner {
		feeType = ssfeeTypeMiner
	}

	// Read the candidate outpoints and close the database transaction before
	// querying the chain. FetchUtxoEntrySKADetails takes the chain lock, while
	// block processing holds the chain lock and waits in db.Flush for every
	// open transaction to finish. Calling into the chain from inside db.View
	// inverts that lock order and deadlocks the node.
	var outpoints []wire.OutPoint
	err := idx.db.View(func(dbTx database.Tx) error {
		// Get the SSFee index bucket
		bucket := dbTx.Metadata().Bucket(ssfeeIndexKey)
		if bucket == nil {
			return fmt.Errorf("ssfee index bucket not found")
		}

		// Create index key for this (feeType, coinType, address)
		key, err := makeSSFeeIndexKey(feeType, coinType, addressHash160)
		if err != nil {
			return fmt.Errorf("failed to create index key: %w", err)
		}

		// Fetch outpoints for this key
		data := bucket.Get(key)
		if data == nil {
			// No UTXOs exist for this address
			log.Debugf("SSFeeIndex: No outpoints found for key=%x (bucket.Get returned nil)", key)
			return nil
		}

		outpoints, err = deserializeOutPoints(data)
		if err != nil {
			return fmt.Errorf("failed to deserialize outpoints: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, 0, 0, err
	}

	// Query blockchain UTXO set to find an unspent output. The outpoints are
	// a snapshot; each one is re-validated against the UTXO set here.
	for _, op := range outpoints {
		// Use SKA-specific method to get *big.Int amount directly (avoids int64 truncation)
		amount, height, index, spent, err := idx.chain.FetchUtxoEntrySKADetails(op)
		if err != nil {
			continue
		}

		// Skip if UTXO doesn't exist or is spent
		if spent || amount == nil || amount.Sign() <= 0 {
			continue
		}

		// Found valid unspent UTXO - return it with fraud proof data
		log.Debugf("SSFeeIndex: Selected outpoint %v with value %v (height=%d, index=%d)",
			op, amount, height, index)
		return &op, amount, height, index, nil
	}

	// No valid UTXO found - return nil (mining will use null input)
	if len(outpoints) > 0 {
		log.Debugf("SSFeeIndex: No valid unspent UTXO found among %d outpoint(s)", len(outpoints))
	}
	return nil, nil, 0, 0, nil
}
