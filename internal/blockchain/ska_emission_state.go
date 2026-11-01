// Copyright (c) 2025 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/database"
)

// SKA emission state management
// This file manages the persistent state for SKA emissions including:
// - Nonces for replay protection
// - Emission flags to prevent duplicate emissions
// - Proper handling of chain reorganizations

const (
	// Database bucket for SKA emission state
	// This is stored in the blockchain database for persistence
	skaStateBucketName = "skaemissionstate"

	// Current version of the on-disk format
	skaStateFormatVersion = 1

	// Meta key for format version
	skaStateVersionKey = "__meta_version__"
)

// SKAEmissionState manages the persistent state for SKA emissions.
// This includes nonces for replay protection and flags to track
// which coin types have already been emitted.
//
// SECURITY: This state is critical for preventing replay attacks
// and duplicate emissions. It must be properly synchronized with
// blockchain state and handle reorganizations correctly.
type SKAEmissionState struct {
	// Protects concurrent access to state
	mtx sync.RWMutex

	// Nonces for each coin type (last successfully used nonce)
	nonces map[cointype.CoinType]uint64

	// Tracks which coin types have been emitted
	emitted map[cointype.CoinType]bool

	// Database handle for persistence
	db database.DB
}

// NewSKAEmissionState creates a new SKA emission state manager.
func NewSKAEmissionState(db database.DB) (*SKAEmissionState, error) {
	state := &SKAEmissionState{
		nonces:  make(map[cointype.CoinType]uint64),
		emitted: make(map[cointype.CoinType]bool),
		db:      db,
	}

	// Load existing state from database
	if err := state.load(); err != nil {
		return nil, fmt.Errorf("failed to load SKA emission state: %w", err)
	}

	return state, nil
}

// GetNonce returns the last used nonce for the specified coin type.
// Returns 0 if no emissions have occurred yet.
func (s *SKAEmissionState) GetNonce(coinType cointype.CoinType) uint64 {
	s.mtx.RLock()
	defer s.mtx.RUnlock()
	return s.nonces[coinType]
}

// IsEmitted returns whether the specified coin type has been emitted.
func (s *SKAEmissionState) IsEmitted(coinType cointype.CoinType) bool {
	s.mtx.RLock()
	defer s.mtx.RUnlock()
	return s.emitted[coinType]
}

// DisconnectSKAEmissionsTx updates the SKA emission state when a block is disconnected,
// using the provided database transaction for atomicity with block updates.
func (s *SKAEmissionState) DisconnectSKAEmissionsTx(dbTx database.Tx, emissions []SKAEmissionRecord) error {
	if len(emissions) == 0 {
		return nil
	}

	s.mtx.Lock()
	defer s.mtx.Unlock()

	// Remove state for each emission
	for _, emission := range emissions {
		// Only remove if this was the emission that set the current nonce
		if currentNonce, exists := s.nonces[emission.CoinType]; exists && currentNonce == emission.Nonce {
			delete(s.nonces, emission.CoinType)
			delete(s.emitted, emission.CoinType)

			log.Debugf("Disconnected SKA emission: coin type %d, nonce %d at height %d",
				emission.CoinType, emission.Nonce, emission.Height)
		}
	}

	// Persist to database using the provided transaction
	return s.saveWithTx(dbTx)
}

// reload replaces the in-memory SKA emission state with the state stored in
// the database.  The in-memory state is left unchanged when it cannot be
// loaded.
func (s *SKAEmissionState) reload() error {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	nonces, emitted := s.nonces, s.emitted
	s.nonces = make(map[cointype.CoinType]uint64)
	s.emitted = make(map[cointype.CoinType]bool)
	if err := s.load(); err != nil {
		s.nonces, s.emitted = nonces, emitted
		return err
	}
	return nil
}

// load reads the SKA emission state from the database.
func (s *SKAEmissionState) load() error {
	err := s.db.View(func(dbTx database.Tx) error {
		bucket := dbTx.Metadata().Bucket([]byte(skaStateBucketName))
		if bucket == nil {
			// No existing state, start fresh
			return nil
		}

		// Check format version first
		var version uint32
		if versionBytes := bucket.Get([]byte(skaStateVersionKey)); versionBytes != nil {
			if len(versionBytes) != 4 {
				return fmt.Errorf("invalid SKA state version encoding: expected 4 bytes, got %d", len(versionBytes))
			}
			version = binary.LittleEndian.Uint32(versionBytes)
		} else {
			// Missing version means v1 (for backwards compatibility)
			version = 1
		}

		// Reject unsupported versions
		if version > skaStateFormatVersion {
			return fmt.Errorf("unsupported SKA state version %d > %d", version, skaStateFormatVersion)
		}

		// Read all entries from the bucket
		return bucket.ForEach(func(k, v []byte) error {
			// Skip meta keys using bytes.Equal for efficiency
			if bytes.Equal(k, []byte(skaStateVersionKey)) {
				return nil
			}

			if len(k) != 1 {
				return fmt.Errorf("invalid key length in SKA state bucket: %d", len(k))
			}

			// Reject invalid coin type 0 (VAR is not an SKA coin)
			if k[0] == 0 {
				return fmt.Errorf("invalid coin type 0 found in SKA state")
			}

			coinType := cointype.CoinType(k[0])

			// Value format: [nonce:8 bytes][emitted:1 byte]
			if len(v) != 9 {
				return fmt.Errorf("invalid value length for coin type %d: %d", coinType, len(v))
			}

			// Parse nonce
			nonce := binary.LittleEndian.Uint64(v[:8])
			s.nonces[coinType] = nonce

			// Parse emitted flag
			if v[8] != 0 {
				s.emitted[coinType] = true
			}

			return nil
		})
	})

	if err != nil {
		return fmt.Errorf("failed to load SKA emission state: %w", err)
	}

	log.Debugf("Loaded SKA emission state: %d coin types tracked", len(s.nonces))
	return nil
}

// saveWithTx writes the SKA emission state using the provided transaction.
// This allows the state to be saved atomically with other blockchain updates.
func (s *SKAEmissionState) saveWithTx(dbTx database.Tx) error {
	meta := dbTx.Metadata()

	// Delete and recreate bucket for clean state (removes any unknown keys)
	if meta.Bucket([]byte(skaStateBucketName)) != nil {
		if err := meta.DeleteBucket([]byte(skaStateBucketName)); err != nil {
			return fmt.Errorf("failed to delete old SKA state bucket: %w", err)
		}
	}

	bucket, err := meta.CreateBucket([]byte(skaStateBucketName))
	if err != nil {
		return fmt.Errorf("failed to create SKA state bucket: %w", err)
	}

	// Write format version
	versionBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(versionBytes, skaStateFormatVersion)
	if err := bucket.Put([]byte(skaStateVersionKey), versionBytes); err != nil {
		return fmt.Errorf("failed to save format version: %w", err)
	}

	// Build a set of all coin types that have state
	// This avoids iterating through all 255 coin types when only a few have data
	coinTypesToSave := make(map[cointype.CoinType]bool)
	for ct := range s.nonces {
		coinTypesToSave[ct] = true
	}
	for ct := range s.emitted {
		coinTypesToSave[ct] = true
	}

	// Save each coin type's state
	for coinType := range coinTypesToSave {
		nonce := s.nonces[coinType]
		isEmitted := s.emitted[coinType]

		// Create key (1 byte coin type)
		key := []byte{byte(coinType)}

		// Create value (8 bytes nonce + 1 byte emitted flag)
		value := make([]byte, 9)
		binary.LittleEndian.PutUint64(value[:8], nonce)
		if isEmitted {
			value[8] = 1
		}

		// Store in bucket
		if err := bucket.Put(key, value); err != nil {
			return fmt.Errorf("failed to save state for coin type %d: %w", coinType, err)
		}
	}

	return nil
}

// Clear removes all SKA emission state from the database.
// This should only be used during database initialization or recovery.
func (s *SKAEmissionState) Clear() error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	// Clear in-memory state
	s.nonces = make(map[cointype.CoinType]uint64)
	s.emitted = make(map[cointype.CoinType]bool)

	// Clear database state
	return s.db.Update(func(dbTx database.Tx) error {
		meta := dbTx.Metadata()

		// Delete the entire bucket if it exists
		if meta.Bucket([]byte(skaStateBucketName)) != nil {
			if err := meta.DeleteBucket([]byte(skaStateBucketName)); err != nil {
				return fmt.Errorf("failed to delete SKA state bucket: %w", err)
			}
		}

		return nil
	})
}

// GetEmissionStateSnapshot returns a copy of the current emission state.
// This is useful for debugging and testing.
func (s *SKAEmissionState) GetEmissionStateSnapshot() (map[cointype.CoinType]uint64, map[cointype.CoinType]bool) {
	s.mtx.RLock()
	defer s.mtx.RUnlock()

	// Create copies to avoid external modification
	noncesCopy := make(map[cointype.CoinType]uint64)
	for k, v := range s.nonces {
		noncesCopy[k] = v
	}

	emittedCopy := make(map[cointype.CoinType]bool)
	for k, v := range s.emitted {
		emittedCopy[k] = v
	}

	return noncesCopy, emittedCopy
}

// SKAEmissionRecord represents a recorded emission in a block.
// This is used during block connection/disconnection to update state.
type SKAEmissionRecord struct {
	CoinType cointype.CoinType
	Nonce    uint64
	Height   int64
	TxHash   [32]byte
}

// ConnectSKAEmissionsTx updates the SKA emission state when a block is connected,
// using the provided database transaction for atomicity with block updates.
func (s *SKAEmissionState) ConnectSKAEmissionsTx(dbTx database.Tx, emissions []SKAEmissionRecord) error {
	if len(emissions) == 0 {
		return nil
	}

	s.mtx.Lock()
	defer s.mtx.Unlock()

	// Update state for each emission
	for _, emission := range emissions {
		s.nonces[emission.CoinType] = emission.Nonce
		s.emitted[emission.CoinType] = true

		log.Debugf("Connected SKA emission: coin type %d, nonce %d at height %d",
			emission.CoinType, emission.Nonce, emission.Height)
	}

	// Persist to database using the provided transaction
	return s.saveWithTx(dbTx)
}
