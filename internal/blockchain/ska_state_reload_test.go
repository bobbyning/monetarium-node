// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"errors"
	"math/big"
	"testing"

	"github.com/monetarium/monetarium-node/database"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/txscript/stdscript"
	"github.com/monetarium/monetarium-node/wire"
)

// TestSKAStateReloadAfterFailedUpdate ensures reloading the SKA emission and
// burn state after a database update that changed them and then failed
// restores the state stored in the database.
func TestSKAStateReloadAfterFailedUpdate(t *testing.T) {
	db, teardown := createTestDB(t, "skastate_reload")
	defer teardown()

	emissionState, err := NewSKAEmissionState(db)
	if err != nil {
		t.Fatalf("NewSKAEmissionState: %v", err)
	}
	burnState, err := NewSKABurnState(db)
	if err != nil {
		t.Fatalf("NewSKABurnState: %v", err)
	}

	// Store an emission and a burn that must survive the failed update.
	stored := SKAEmissionRecord{CoinType: 2, Nonce: 7, Height: 99}
	burn := SKABurnRecord{CoinType: 1, Amount: bigInt(500), Height: 100}
	err = db.Update(func(dbTx database.Tx) error {
		err := emissionState.ConnectSKAEmissionsTx(dbTx,
			[]SKAEmissionRecord{stored})
		if err != nil {
			return err
		}
		return burnState.ConnectSKABurnsTx(dbTx, []SKABurnRecord{burn})
	})
	if err != nil {
		t.Fatalf("store state: %v", err)
	}

	// Change both states in an update that then fails and is rolled back.
	errFail := errors.New("forced failure")
	err = db.Update(func(dbTx database.Tx) error {
		emission := SKAEmissionRecord{CoinType: 1, Nonce: 1, Height: 101}
		err := emissionState.ConnectSKAEmissionsTx(dbTx,
			[]SKAEmissionRecord{emission})
		if err != nil {
			return err
		}
		err = burnState.DisconnectSKABurnsTx(dbTx, []SKABurnRecord{burn})
		if err != nil {
			return err
		}
		newBurn := SKABurnRecord{CoinType: 2, Amount: bigInt(300), Height: 101}
		err = burnState.ConnectSKABurnsTx(dbTx, []SKABurnRecord{newBurn})
		if err != nil {
			return err
		}
		return errFail
	})
	if !errors.Is(err, errFail) {
		t.Fatalf("update: got %v, want %v", err, errFail)
	}
	if !emissionState.IsEmitted(1) || burnState.GetBurnedAmount(2) == nil {
		t.Fatal("precondition: the failed update did not change the " +
			"in-memory state")
	}

	// checkStored ensures the in-memory state is the stored state.
	checkStored := func(when string) {
		t.Helper()
		if emissionState.IsEmitted(1) || emissionState.GetNonce(1) != 0 {
			t.Fatalf("SKA1 emission %s: emitted %v, nonce %d", when,
				emissionState.IsEmitted(1), emissionState.GetNonce(1))
		}
		if !emissionState.IsEmitted(2) || emissionState.GetNonce(2) != 7 {
			t.Fatalf("SKA2 emission %s: emitted %v, nonce %d, want "+
				"emitted, nonce 7", when, emissionState.IsEmitted(2),
				emissionState.GetNonce(2))
		}
		if got := burnState.GetBurnedAmount(1); got == nil || got.Cmp(bigInt(500)) != 0 {
			t.Fatalf("SKA1 burned amount %s: got %v, want 500", when, got)
		}
		if got := burnState.GetBurnedAmount(2); got != nil {
			t.Fatalf("SKA2 burned amount %s: got %v, want none", when, got)
		}
	}

	if err := emissionState.reload(); err != nil {
		t.Fatalf("emission state reload: %v", err)
	}
	if err := burnState.reload(); err != nil {
		t.Fatalf("burn state reload: %v", err)
	}
	checkStored("after reload")

	// A reload that cannot read the database leaves the in-memory state as
	// it was.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := emissionState.reload(); err == nil {
		t.Fatal("emission state reload from a closed database succeeded")
	}
	if err := burnState.reload(); err == nil {
		t.Fatal("burn state reload from a closed database succeeded")
	}
	checkStored("after failed reload")
}

// errForcedUpdate is the error failUpdateDB fails an update with.
var errForcedUpdate = errors.New("forced update failure")

// failUpdateDB wraps a database so that an update fails and is rolled back
// when fail, called after the update function succeeds, returns true.
type failUpdateDB struct {
	database.DB
	fail func() bool
}

// Update runs fn in a database update and fails the update when fail returns
// true after fn succeeds.
func (db *failUpdateDB) Update(fn func(database.Tx) error) error {
	return db.DB.Update(func(dbTx database.Tx) error {
		if err := fn(dbTx); err != nil {
			return err
		}
		if db.fail() {
			return errForcedUpdate
		}
		return nil
	})
}

// TestSKAStateReloadAfterFailedBlockUpdate ensures a failed database update
// while connecting or disconnecting a block leaves the in-memory SKA emission
// and burn state matching the database.
func TestSKAStateReloadAfterFailedBlockUpdate(t *testing.T) {
	// newEmitBlock returns a harness whose tip block "bemit" holds an SKA1
	// emission and has not been processed yet.
	newEmitBlock := func(t *testing.T) *chaingenHarness {
		g, emission := newSKAEmissionRuleHarness(t, 1, 100)
		height := int64(g.Tip().Header.Height) + 1
		outs := g.OldestCoinbaseOuts()
		g.NextBlock("bemit", nil, outs[1:], func(b *wire.MsgBlock) {
			b.AddTransaction(emission(height, 1))
		})
		return g
	}

	t.Run("Connect", func(t *testing.T) {
		g := newEmitBlock(t)
		state := g.chain.skaEmissionState

		// Fail the update that records the emission.
		g.chain.db = &failUpdateDB{DB: g.chain.db, fail: func() bool {
			return state.IsEmitted(1)
		}}
		_, err := g.chain.ProcessBlock(dcrutil.NewBlock(g.Tip()))
		if !errors.Is(err, errForcedUpdate) {
			t.Fatalf("connect: got %v, want %v", err, errForcedUpdate)
		}
		if state.IsEmitted(1) || state.GetNonce(1) != 0 {
			t.Fatalf("after failed connect: emitted %v, nonce %d, want "+
				"not emitted, nonce 0", state.IsEmitted(1), state.GetNonce(1))
		}
	})

	t.Run("Disconnect", func(t *testing.T) {
		g := newEmitBlock(t)
		g.AcceptTipBlock()
		outs := g.OldestCoinbaseOuts()
		g.NextBlock("b1", nil, outs[1:])
		g.AcceptTipBlock()
		state := g.chain.skaEmissionState
		burnState := g.chain.skaBurnState

		// Disconnecting the emission block itself fails earlier, while
		// reading its spend journal entry, so disconnect a block on top of
		// it and have the failed update drop the emission from memory as a
		// rolled back disconnect of the emission block would, and record a
		// burn as a rolled back burn would.
		g.chain.db = &failUpdateDB{DB: g.chain.db, fail: func() bool {
			state.mtx.Lock()
			delete(state.nonces, 1)
			delete(state.emitted, 1)
			state.mtx.Unlock()
			burnState.mtx.Lock()
			burnState.burned[1] = bigInt(500)
			burnState.mtx.Unlock()
			return true
		}}
		hash := g.Tip().BlockHash()
		err := g.chain.InvalidateBlock(&hash)
		if !errors.Is(err, errForcedUpdate) {
			t.Fatalf("disconnect: got %v, want %v", err, errForcedUpdate)
		}
		if !state.IsEmitted(1) || state.GetNonce(1) != 1 {
			t.Fatalf("after failed disconnect: emitted %v, nonce %d, want "+
				"emitted, nonce 1", state.IsEmitted(1), state.GetNonce(1))
		}
		if got := burnState.GetBurnedAmount(1); got != nil {
			t.Fatalf("burned amount after failed disconnect: got %v, want "+
				"none", got)
		}
	})
	// newBurnBlock returns a harness whose tip block "bburn" burns
	// burnAmount of SKA1 and has not been processed yet.
	const burnAmount = 100
	newBurnBlock := func(t *testing.T) *skaDisconnectHarness {
		h := newSKADisconnectHarness(t)
		burnTx := h.createSKASpendTx()
		burnTx.TxOut[0].SKAValue.Sub(burnTx.TxOut[0].SKAValue,
			bigInt(burnAmount))
		burnTx.AddTxOut(&wire.TxOut{
			SKAValue: bigInt(burnAmount),
			CoinType: 1,
			PkScript: stdscript.NewSKABurnScriptV0(1),
		})
		outs := h.OldestCoinbaseOuts()
		h.NextBlock("bburn", nil, outs[1:], func(b *wire.MsgBlock) {
			addSKASpendTx(b, burnTx)
		})
		return h
	}

	// checkBurned ensures the in-memory and the stored SKA1 burned amount
	// are both want, or both none when want is nil.
	checkBurned := func(t *testing.T, h *skaDisconnectHarness, db database.DB,
		when string, want *big.Int) {

		t.Helper()
		stored, err := NewSKABurnState(db)
		if err != nil {
			t.Fatalf("NewSKABurnState: %v", err)
		}
		for _, got := range []struct {
			what   string
			amount *big.Int
		}{
			{"in memory", h.chain.skaBurnState.GetBurnedAmount(1)},
			{"stored", stored.GetBurnedAmount(1)},
		} {
			if (got.amount == nil) != (want == nil) ||
				(want != nil && got.amount.Cmp(want) != 0) {
				t.Fatalf("%s: burned %s %v, want %v", when, got.what,
					got.amount, want)
			}
		}
	}

	t.Run("ConnectBurn", func(t *testing.T) {
		h := newBurnBlock(t)
		db, burnState := h.chain.db, h.chain.skaBurnState

		// Fail the update that records the burn.
		h.chain.db = &failUpdateDB{DB: db, fail: func() bool {
			return burnState.GetBurnedAmount(1) != nil
		}}
		_, err := h.chain.ProcessBlock(dcrutil.NewBlock(h.Tip()))
		if !errors.Is(err, errForcedUpdate) {
			t.Fatalf("connect: got %v, want %v", err, errForcedUpdate)
		}
		checkBurned(t, h, db, "after failed connect", nil)
	})

	t.Run("DisconnectBurn", func(t *testing.T) {
		h := newBurnBlock(t)
		h.AcceptTipBlock()
		db, burnState := h.chain.db, h.chain.skaBurnState
		checkBurned(t, h, db, "after connect", bigInt(burnAmount))

		// Fail the update that removes the burn.
		h.chain.db = &failUpdateDB{DB: db, fail: func() bool {
			return burnState.GetBurnedAmount(1) == nil
		}}
		hash := h.Tip().BlockHash()
		err := h.chain.InvalidateBlock(&hash)
		if !errors.Is(err, errForcedUpdate) {
			t.Fatalf("disconnect: got %v, want %v", err, errForcedUpdate)
		}
		checkBurned(t, h, db, "after failed disconnect", bigInt(burnAmount))
	})
}
