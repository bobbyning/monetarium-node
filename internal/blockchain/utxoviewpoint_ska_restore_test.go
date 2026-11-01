// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"math/big"
	"testing"

	"github.com/monetarium/monetarium-node/blockchain/stake"
	"github.com/monetarium/monetarium-node/chaincfg/chainhash"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/wire"
)

// TestDisconnectRestoresSpentAmount ensures an output restored when the
// transaction spending it is disconnected carries the amounts of the output,
// whatever the spending input's ValueIn holds: an SKA output its SKA amount
// and no VAR amount, a VAR output its VAR amount.  Each output is spent from
// the regular tree or by an augmented SSFee in the stake tree, and the spend is
// disconnected both from the spend journal and from the spent txouts recorded
// at connect time.
func TestDisconnectRestoresSpentAmount(t *testing.T) {
	pkScript := hexToBytes("76a9146edbc6c4d31bae9f1ccc38538a114bf42de65e8688ac")
	skaOut := wire.TxOut{CoinType: 1, PkScript: pkScript,
		SKAValue: new(big.Int).Mul(big.NewInt(5), big.NewInt(1e18))}
	varOut := wire.TxOut{Value: 3e8, PkScript: pkScript}

	tests := []struct {
		name      string
		stakeTree bool
		prevOut   wire.TxOut // the output the spend transaction spends
		valueIn   int64      // ValueIn of the spending input
	}{{
		name:    "SKA regular tree",
		prevOut: skaOut,
		valueIn: 7e8,
	}, {
		name:      "SKA stake tree",
		stakeTree: true,
		prevOut:   skaOut,
		valueIn:   7e8,
	}, {
		name:    "VAR regular tree",
		prevOut: varOut,
		valueIn: varOut.Value,
	}, {
		name:      "VAR stake tree",
		stakeTree: true,
		prevOut:   varOut,
		valueIn:   varOut.Value,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// newTx returns a transaction with txIn paying test.prevOut, as a
			// staker SSFee when the case spends from the stake tree.
			newTx := func(txIn *wire.TxIn) *wire.MsgTx {
				tx := wire.NewMsgTx()
				tx.AddTxIn(txIn)
				if test.stakeTree {
					tx.Version = 3
					tx.AddTxOut(&wire.TxOut{CoinType: test.prevOut.CoinType,
						SKAValue: new(big.Int),
						PkScript: stake.CreateStakerSSFeeMarker(100, 0)})
				}
				out := test.prevOut
				tx.AddTxOut(&out)
				return tx
			}

			// create holds the output that spend spends.  In the stake tree
			// it is a null-input SSFee, which spend augments.
			create := newTx(&wire.TxIn{PreviousOutPoint: wire.OutPoint{
				Hash: chainhash.Hash{0x09}, Index: wire.MaxPrevOutIndex}})
			tree := wire.TxTreeRegular
			if test.stakeTree {
				tree = wire.TxTreeStake
			}
			prevOut := wire.OutPoint{Hash: create.TxHash(),
				Index: uint32(len(create.TxOut) - 1), Tree: tree}
			spend := newTx(&wire.TxIn{PreviousOutPoint: prevOut,
				ValueIn: test.valueIn, SKAValueIn: test.prevOut.SKAValue,
				BlockHeight: 100, BlockIndex: 1})
			if test.stakeTree && stake.DetermineTxType(spend) !=
				stake.TxTypeSSFee {

				t.Fatal("spend is not an SSFee")
			}

			coinbase := wire.NewMsgTx()
			coinbase.AddTxIn(&wire.TxIn{
				PreviousOutPoint: wire.OutPoint{Index: wire.MaxPrevOutIndex},
				SignatureScript:  []byte{0x00, 0x00},
			})
			coinbase.AddTxOut(&wire.TxOut{Value: 1, PkScript: pkScript})
			msgBlock := &wire.MsgBlock{
				Header:       wire.BlockHeader{Height: 200},
				Transactions: []*wire.MsgTx{coinbase},
			}
			if test.stakeTree {
				msgBlock.STransactions = []*wire.MsgTx{spend}
			} else {
				msgBlock.Transactions = append(msgBlock.Transactions, spend)
			}
			block := dcrutil.NewBlock(msgBlock)

			// connect and disconnect run the tree of the case.
			connect := (*UtxoViewpoint).connectRegularTransactions
			disconnect := (*UtxoViewpoint).disconnectRegularTransactions
			if test.stakeTree {
				connect = (*UtxoViewpoint).connectStakeTransactions
				disconnect = (*UtxoViewpoint).disconnectStakeTransactions
			}

			view := NewUtxoViewpoint(nil)
			view.AddTxOuts(dcrutil.NewTx(create), 100, 1, false)
			var stxos []spentTxOut
			if err := connect(view, block, &stxos, false); err != nil {
				t.Fatalf("connect: %v", err)
			}
			serialized, err := serializeSpendJournalEntry(stxos)
			if err != nil {
				t.Fatalf("serialize: %v", err)
			}
			journal, err := deserializeSpendJournalEntry(serialized,
				[]*wire.MsgTx{spend})
			if err != nil {
				t.Fatalf("deserialize: %v", err)
			}

			sources := []struct {
				name  string
				stxos []spentTxOut
			}{
				{name: "spend journal", stxos: journal},
				{name: "connect stxos", stxos: stxos},
			}
			for _, src := range sources {
				view := NewUtxoViewpoint(nil)
				err := disconnect(view, block, src.stxos, false)
				if err != nil {
					t.Fatalf("%s: disconnect: %v", src.name, err)
				}
				entry := view.entries[prevOut]
				if entry == nil || entry.IsSpent() {
					t.Fatalf("%s: spent output not restored", src.name)
				}
				if got, want := entry.Amount(), test.prevOut.Value; got != want {
					t.Fatalf("%s: restored VAR amount %d, want %d", src.name,
						got, want)
				}
				got, want := entry.SKAAmount(), test.prevOut.SKAValue
				if (got == nil) != (want == nil) ||
					(got != nil && got.Cmp(want) != 0) {

					t.Fatalf("%s: restored SKA amount %v, want %v", src.name,
						got, want)
				}
				if got, want := entry.CoinType(), test.prevOut.CoinType; got != want {
					t.Fatalf("%s: restored coin type %d, want %d", src.name,
						got, want)
				}
			}
		})
	}
}
