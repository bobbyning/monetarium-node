// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"bytes"
	"fmt"
	"math/big"
	"testing"

	"github.com/monetarium/monetarium-node/blockchain/chaingen"
	"github.com/monetarium/monetarium-node/blockchain/stake"
	"github.com/monetarium/monetarium-node/blockchain/standalone"
	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/chaincfg/chainhash"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrec/secp256k1"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/txscript"
	"github.com/monetarium/monetarium-node/txscript/stdaddr"
	"github.com/monetarium/monetarium-node/wire"
)

// skaDisconnectHarness bundles a chaingen harness that has emitted SKA1 to a
// p2sh OP_TRUE output, so the test can spend it without tracking keys.
type skaDisconnectHarness struct {
	*chaingenHarness
	emitAmount *big.Int
	emitOut    wire.OutPoint // the emitted SKA output
	emitIn     wire.OutPoint // the null outpoint of the emission's input
	preEmit    string        // name of the parent of block "bemit"

	// spent is the output spent by the regular transaction block "bemit"
	// holds besides the emission and spentEntry is its utxo entry before the
	// block connected.  Both are nil when the emission is the only one.
	spent      *wire.OutPoint
	spentEntry *UtxoEntry
}

// emitPlacement is where block "bemit" holds the SKA emission relative to a
// regular transaction spending an older coinbase output.
type emitPlacement int

const (
	emitAlone       emitPlacement = iota // no spending transaction
	emitBeforeSpend                      // the emission directly follows the coinbase
	emitAfterSpend                       // the emission is the last transaction
)

// newSKADisconnectHarness returns a regnet harness advanced past stake
// validation height whose block "bemit" holds an SKA1 emission paying the
// generator's p2sh OP_TRUE address, with the tip at the height where that
// output is mature.
func newSKADisconnectHarness(t *testing.T) *skaDisconnectHarness {
	t.Helper()
	h := newSKAEmissionHarness(t, emitAlone)
	h.advanceToEmissionMaturity()
	return h
}

// newSKAEmissionHarness returns a regnet harness advanced past stake
// validation height whose tip block "bemit" holds an SKA1 emission paying the
// generator's p2sh OP_TRUE address, placed as requested.  A spending
// transaction spends an output of a different block than the tickets of
// "bemit" do, so its spend journal entry differs from theirs.
func newSKAEmissionHarness(t *testing.T, placement emitPlacement) *skaDisconnectHarness {
	t.Helper()

	params := cloneParams(chaincfg.RegNetParams())
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("failed to generate emission key: %v", err)
	}
	opTrueAddr, err := stdaddr.NewAddressScriptHashV0([]byte{txscript.OP_TRUE},
		params)
	if err != nil {
		t.Fatalf("failed to create p2sh OP_TRUE address: %v", err)
	}
	emitAmount := new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))
	emitHeight := params.StakeValidationHeight + 1
	params.SKACoins = map[cointype.CoinType]*chaincfg.SKACoinConfig{
		1: {
			CoinType:          1,
			Name:              "Skarb-1",
			Symbol:            "SKA1",
			MaxSupply:         emitAmount,
			AtomsPerCoin:      big.NewInt(1e18),
			EmissionHeight:    int32(emitHeight),
			EmissionWindow:    100,
			Active:            true,
			EmissionAddresses: []string{opTrueAddr.String()},
			EmissionAmounts:   []*big.Int{emitAmount},
			EmissionKey:       priv.PubKey(),
		},
	}
	params.InitialSKATypes = []cointype.CoinType{1}

	g := newChaingenHarness(t, params)
	g.AdvanceToStakeValidationHeight()

	emitTx := signedSKA1EmissionTx(t, params, priv,
		params.SKACoins[1].EmissionAddresses, []*big.Int{emitAmount},
		emitHeight)
	emitTx.TxIn[0].BlockHeight = wire.NullBlockHeight
	emitTx.TxIn[0].BlockIndex = wire.NullBlockIndex
	preEmit := g.TipName()
	var spend *chaingen.SpendableOut
	var spent *wire.OutPoint
	var spentEntry *UtxoEntry
	if placement != emitAlone {
		spend = &g.OldestCoinbaseOuts()[0]
		prevOut := spend.PrevOut()
		entry, err := g.chain.FetchUtxoEntry(prevOut)
		if err != nil || entry == nil {
			t.Fatalf("FetchUtxoEntry(%v): entry %v, err %v", prevOut, entry, err)
		}
		spent, spentEntry = &prevOut, entry.Clone()
	}
	outs := g.OldestCoinbaseOuts()
	g.NextBlock("bemit", spend, outs[1:], func(b *wire.MsgBlock) {
		if placement == emitBeforeSpend {
			b.Transactions = append(b.Transactions[:1],
				append([]*wire.MsgTx{emitTx}, b.Transactions[1:]...)...)
			return
		}
		b.AddTransaction(emitTx)
	})
	g.SaveTipCoinbaseOuts()
	g.AcceptTipBlock()

	return &skaDisconnectHarness{
		chaingenHarness: g,
		emitAmount:      emitAmount,
		preEmit:         preEmit,
		emitOut: wire.OutPoint{
			Hash:  emitTx.TxHash(),
			Index: 0,
			Tree:  wire.TxTreeRegular,
		},
		emitIn:     emitTx.TxIn[0].PreviousOutPoint,
		spent:      spent,
		spentEntry: spentEntry,
	}
}

// advanceToEmissionMaturity extends the chain until the emitted output is
// mature.  Outputs of transactions with an expiry, such as the emission,
// require coinbase maturity before they can be spent.
func (h *skaDisconnectHarness) advanceToEmissionMaturity() {
	for i := uint16(0); i < h.Params().CoinbaseMaturity; i++ {
		outs := h.OldestCoinbaseOuts()
		h.NextBlock(fmt.Sprintf("bskamat%d", i), nil, outs[1:])
		h.SaveTipCoinbaseOuts()
		h.AcceptTipBlock()
	}
}

// skaSpendFee is the SKA fee, in atoms, paid by the spend transaction.
const skaSpendFee = 10

// createSKASpendTx returns a transaction that spends the emitted SKA output in
// to a new p2sh OP_TRUE SKA output, paying skaSpendFee.
func (h *skaDisconnectHarness) createSKASpendTx() *wire.MsgTx {
	emitBlock := h.BlockByName("bemit")
	scriptVer, script := h.P2shOpTrueAddr().PaymentScript()
	tx := wire.NewMsgTx()
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: h.emitOut,
		Sequence:         wire.MaxTxInSequenceNum,
		SKAValueIn:       new(big.Int).Set(h.emitAmount),
		BlockHeight:      emitBlock.Header.Height,
		BlockIndex:       uint32(len(emitBlock.Transactions) - 1),
		SignatureScript:  []byte{txscript.OP_DATA_1, txscript.OP_TRUE},
	})
	tx.AddTxOut(&wire.TxOut{
		SKAValue: new(big.Int).Sub(h.emitAmount, big.NewInt(skaSpendFee)),
		CoinType: 1,
		Version:  scriptVer,
		PkScript: script,
	})
	return tx
}

// addSKASpendTx adds spendTx to the regular tree of b along with the stake
// tree SSFee transactions that distribute its SKA fee to the block's voters
// and miner.
func addSKASpendTx(b *wire.MsgBlock, spendTx *wire.MsgTx) {
	b.AddTransaction(spendTx)

	height := int64(b.Header.Height)
	minerFee, stakerFee := standalone.CalcFeeSplit(skaSpendFee,
		standalone.SSVMonetarium)

	// newSSFeeTx returns a null-input SSFee transaction with the given marker
	// paying amount to pkScript.
	newSSFeeTx := func(marker, pkScript []byte, amount int64) *wire.MsgTx {
		tx := wire.NewMsgTx()
		tx.Version = 3
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: *wire.NewOutPoint(&chainhash.Hash{},
				wire.MaxPrevOutIndex, wire.TxTreeRegular),
			Sequence:    wire.MaxTxInSequenceNum,
			BlockHeight: wire.NullBlockHeight,
			BlockIndex:  wire.NullBlockIndex,
		})
		tx.AddTxOut(&wire.TxOut{
			SKAValue: new(big.Int),
			CoinType: 1,
			PkScript: marker,
		})
		tx.AddTxOut(&wire.TxOut{
			SKAValue: big.NewInt(amount),
			CoinType: 1,
			PkScript: pkScript,
		})
		return tx
	}

	// Staker share.  Every chaingen vote names the same consolidation address,
	// so the whole share goes to it in a single SSFee keyed by the first vote.
	hash160, err := stake.ExtractSSFeeConsolidationAddr(b.STransactions[0])
	if err != nil {
		panic(err)
	}
	pkScript, err := stake.ConsolidationAddrToPkScript(hash160)
	if err != nil {
		panic(err)
	}
	b.AddSTransaction(newSSFeeTx(stake.CreateStakerSSFeeMarker(height, 0),
		pkScript, stakerFee))

	// Miner share to an OP_SSGEN-tagged anyone-can-spend script.
	b.AddSTransaction(newSSFeeTx(stake.CreateMinerSSFeeMarker(height),
		[]byte{txscript.OP_SSGEN, txscript.OP_TRUE}, minerFee))
}

// assertSKASpendUndone ensures the utxo set at the current tip no longer
// holds the output created by spendTx and holds the emitted output it spent
// as unspent with its original SKA amount and coin type.
func (h *skaDisconnectHarness) assertSKASpendUndone(spendTx *wire.MsgTx) {
	t := h.t
	t.Helper()

	created := wire.OutPoint{Hash: spendTx.TxHash(), Tree: wire.TxTreeRegular}
	entry, err := h.chain.FetchUtxoEntry(created)
	if err != nil {
		t.Fatalf("FetchUtxoEntry(%v): %v", created, err)
	}
	if entry != nil && !entry.IsSpent() {
		t.Errorf("SKA output %v created by the undone transaction is still "+
			"unspent in the utxo set (coin type %d, SKA amount %v)", created,
			entry.CoinType(), entry.SKAAmount())
	}

	entry, err = h.chain.FetchUtxoEntry(h.emitOut)
	if err != nil {
		t.Fatalf("FetchUtxoEntry(%v): %v", h.emitOut, err)
	}
	if entry == nil || entry.IsSpent() {
		t.Fatalf("SKA output %v spent by the undone transaction was not "+
			"restored to the utxo set", h.emitOut)
	}
	if got := entry.CoinType(); got != 1 {
		t.Fatalf("restored output %v coin type: got %d, want 1", h.emitOut, got)
	}
	if got := entry.SKAAmount(); got == nil || got.Cmp(h.emitAmount) != 0 {
		t.Fatalf("restored output %v SKA amount: got %v, want %v", h.emitOut,
			got, h.emitAmount)
	}
}

// assertSSFeeOutputsGone ensures the utxo set at the current tip holds none of
// the SKA outputs created by the null-input SSFee transactions in the stake
// tree of the disconnected block b.
func (h *skaDisconnectHarness) assertSSFeeOutputsGone(b *wire.MsgBlock) {
	t := h.t
	t.Helper()

	var checked int
	for _, stx := range b.STransactions {
		if stake.DetermineTxType(stx) != stake.TxTypeSSFee {
			continue
		}
		for i, txOut := range stx.TxOut {
			if !txOut.CoinType.IsSKA() || txOut.SKAValue == nil ||
				txOut.SKAValue.Sign() == 0 {
				continue
			}
			op := wire.OutPoint{Hash: stx.TxHash(), Index: uint32(i),
				Tree: wire.TxTreeStake}
			entry, err := h.chain.FetchUtxoEntry(op)
			if err != nil {
				t.Fatalf("FetchUtxoEntry(%v): %v", op, err)
			}
			if entry != nil && !entry.IsSpent() {
				t.Errorf("SKA SSFee output %v of the disconnected block is "+
					"still unspent in the utxo set (SKA amount %v)", op,
					entry.SKAAmount())
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("disconnected block has no SKA SSFee outputs to check")
	}
}

// assertUtxoGone ensures the utxo set at the current tip does not hold the
// passed output as unspent.
func (h *skaDisconnectHarness) assertUtxoGone(op wire.OutPoint) {
	h.t.Helper()
	entry, err := h.chain.FetchUtxoEntry(op)
	if err != nil {
		h.t.Fatalf("FetchUtxoEntry(%v): %v", op, err)
	}
	if entry != nil && !entry.IsSpent() {
		h.t.Fatalf("output %v of the undone block is still unspent in the "+
			"utxo set (coin type %d, SKA amount %v)", op, entry.CoinType(),
			entry.SKAAmount())
	}
}

// flushUtxoCache forces the utxo cache to the backend so the following
// lookups read the persisted state.
func (h *skaDisconnectHarness) flushUtxoCache() {
	h.t.Helper()
	tip := h.chain.bestChain.Tip()
	err := h.chain.utxoCache.MaybeFlush(&tip.hash, uint32(tip.height), true,
		false)
	if err != nil {
		h.t.Fatalf("failed to flush utxo cache: %v", err)
	}
}

// TestDisconnectSKAOutputs ensures that undoing the regular transactions of a
// block removes the SKA outputs those transactions created and restores the
// SKA outputs they spent, both when the next block disapproves the regular
// tree and when the block is disconnected by a reorganization.
func TestDisconnectSKAOutputs(t *testing.T) {
	const (
		// vbDisapprovePrev and vbApprovePrev represent no and yes votes,
		// respectively, on whether or not to approve the previous block.
		vbDisapprovePrev = 0x0000
		vbApprovePrev    = 0x0001
	)

	t.Run("Disapproval", func(t *testing.T) {
		h := newSKADisconnectHarness(t)

		//   ... -> bskamat# -> bx (spends the SKA emission)
		spendTx := h.createSKASpendTx()
		outs := h.OldestCoinbaseOuts()
		h.NextBlock("bx", nil, outs[1:], func(b *wire.MsgBlock) {
			addSKASpendTx(b, spendTx)
		})
		h.SaveTipCoinbaseOuts()
		h.AcceptTipBlock()

		//   ... -> bskamat# -> bx -> by (disapproves bx)
		outs = h.OldestCoinbaseOuts()
		h.NextBlock("by", nil, outs[1:], func(b *wire.MsgBlock) {
			b.Header.VoteBits &^= vbApprovePrev
			h.ReplaceVoteBits(vbDisapprovePrev)(b)
		})
		h.AssertTipDisapprovesPrevious()
		h.SaveTipCoinbaseOuts()
		h.AcceptTipBlock()

		h.assertSKASpendUndone(spendTx)
		if t.Failed() {
			return
		}
		h.flushUtxoCache()
		h.assertSKASpendUndone(spendTx)
	})

	t.Run("Reorg", func(t *testing.T) {
		h := newSKADisconnectHarness(t)
		forkName := h.TipName()

		//   ... -> bskamat# -> bx (spends the SKA emission) -> by
		spendTx := h.createSKASpendTx()
		outs := h.OldestCoinbaseOuts()
		bx := h.NextBlock("bx", nil, outs[1:], func(b *wire.MsgBlock) {
			addSKASpendTx(b, spendTx)
		})
		h.AcceptTipBlock()
		h.NextBlock("by", nil, nil)
		h.AcceptTipBlock()

		//   ... -> bskamat# -> bx -> by
		//             \-> bx2 -> by2 -> bz2
		h.SetTip(forkName)
		h.NextBlock("bx2", nil, nil)
		h.AcceptedToSideChainWithExpectedTip("by")
		h.NextBlock("by2", nil, nil)
		h.AcceptedToSideChainWithExpectedTip("by")
		h.NextBlock("bz2", nil, nil)
		h.AcceptTipBlock()

		h.assertSKASpendUndone(spendTx)
		h.assertSSFeeOutputsGone(bx)
		if t.Failed() {
			return
		}
		h.flushUtxoCache()
		h.assertSKASpendUndone(spendTx)
		h.assertSSFeeOutputsGone(bx)
	})
}

// TestDisconnectUnspendableOutputs ensures that undoing the regular
// transactions of a block leaves out of the view each output the connect path
// never added to the utxo set, for both SKA and VAR, while still marking the
// spendable output of the same transaction spent.
//
// Skipping an output and adding it to the view spent leave the same utxo set
// once the view is committed, so the cases inspect the view right after the
// disapproval of the block disconnects its regular tree.
func TestDisconnectUnspendableOutputs(t *testing.T) {
	tests := []struct {
		name  string
		txOut *wire.TxOut // appended to the spend transaction of its coin type
	}{{
		name:  "SKA zero amount",
		txOut: &wire.TxOut{SKAValue: new(big.Int), CoinType: 1, PkScript: []byte{txscript.OP_TRUE}},
	}, {
		name:  "SKA OP_RETURN",
		txOut: &wire.TxOut{SKAValue: big.NewInt(1), CoinType: 1, PkScript: []byte{txscript.OP_RETURN}},
	}, {
		name: "SKA script over max size",
		txOut: &wire.TxOut{SKAValue: big.NewInt(1), CoinType: 1,
			PkScript: bytes.Repeat([]byte{txscript.OP_TRUE}, txscript.MaxScriptSize+1)},
	}, {
		name:  "VAR zero amount",
		txOut: &wire.TxOut{PkScript: []byte{txscript.OP_TRUE}},
	}, {
		name:  "VAR OP_RETURN",
		txOut: &wire.TxOut{Value: 1, PkScript: []byte{txscript.OP_RETURN}},
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newSKADisconnectHarness(t)

			// The output takes its amount from the first output of its
			// transaction, so the fees stay those of the passing layout.
			//
			//   ... -> bskamat# -> bx (spends a coinbase output and the SKA
			//                          emission)
			spendTx := h.createSKASpendTx()
			var tx *wire.MsgTx
			outs := h.OldestCoinbaseOuts()
			h.NextBlock("bx", &outs[0], outs[1:], func(b *wire.MsgBlock) {
				tx = b.Transactions[1]
				if test.txOut.CoinType.IsSKA() {
					tx = spendTx
					tx.TxOut[0].SKAValue.Sub(tx.TxOut[0].SKAValue,
						test.txOut.SKAValue)
				}
				tx.TxOut[0].Value -= test.txOut.Value
				tx.AddTxOut(test.txOut)
				addSKASpendTx(b, spendTx)
			})
			h.AcceptTipBlock()

			// Fetch the view of tx with the regular tree of the tip bx undone,
			// as a block disapproving bx sees it.
			view, err := h.chain.FetchUtxoView(dcrutil.NewTx(tx), false)
			if err != nil {
				t.Fatalf("FetchUtxoView: %v", err)
			}

			txHash := tx.TxHash()
			spendable := wire.OutPoint{Hash: txHash, Tree: wire.TxTreeRegular}
			if entry := view.LookupEntry(spendable); entry == nil ||
				!entry.IsSpent() {
				t.Fatalf("spendable output %v of the undone transaction is "+
					"not marked spent in the view", spendable)
			}
			unspendable := wire.OutPoint{Hash: txHash,
				Index: uint32(len(tx.TxOut) - 1), Tree: wire.TxTreeRegular}
			if entry := view.LookupEntry(unspendable); entry != nil {
				t.Errorf("provably unspendable output %v of the undone "+
					"transaction has a view entry (spent %v)", unspendable,
					entry.IsSpent())
			}
		})
	}
}

// utxoMeta describes the fields of a utxo entry that undoing its spend
// restores from the spend journal.
func utxoMeta(entry *UtxoEntry) string {
	return fmt.Sprintf("coin type %d, amount %d, script %x (version %d), "+
		"height %d, index %d, coinbase %v, expiry %v, tx type %v",
		entry.CoinType(), entry.Amount(), entry.PkScript(),
		entry.ScriptVersion(), entry.BlockHeight(), entry.BlockIndex(),
		entry.IsCoinBase(), entry.HasExpiry(), entry.TransactionType())
}

// assertSKAEmissionUndone ensures the utxo set at the current tip holds neither
// the output of the undone SKA emission nor an entry for its null input, and
// holds the output spent by the other transaction of block "bemit", if any,
// unspent with the metadata it had before the block connected.
func (h *skaDisconnectHarness) assertSKAEmissionUndone() {
	t := h.t
	t.Helper()

	h.assertUtxoGone(h.emitOut)

	entry, err := h.chain.FetchUtxoEntry(h.emitIn)
	if err != nil {
		t.Fatalf("FetchUtxoEntry(%v): %v", h.emitIn, err)
	}
	if entry != nil && !entry.IsSpent() {
		t.Errorf("undoing the SKA emission added its null input %v to the "+
			"utxo set (%s)", h.emitIn, utxoMeta(entry))
	}

	if h.spent == nil {
		return
	}
	entry, err = h.chain.FetchUtxoEntry(*h.spent)
	if err != nil {
		t.Fatalf("FetchUtxoEntry(%v): %v", *h.spent, err)
	}
	if entry == nil || entry.IsSpent() {
		t.Errorf("output %v spent by the undone block was not restored to "+
			"the utxo set", *h.spent)
		return
	}
	if got, want := utxoMeta(entry), utxoMeta(h.spentEntry); got != want {
		t.Errorf("restored output %v metadata:\ngot  %s\nwant %s", *h.spent,
			got, want)
	}
}

// TestDisconnectSKAEmissionBlock ensures a block holding an SKA emission can be
// undone, both when the next block disapproves its regular tree and when it is
// disconnected by a reorganization, wherever the emission sits relative to a
// transaction spending an output.  The emitted output must be removed, the
// emission's null input must not enter the utxo set, and the spent output must
// be restored with its own metadata.
func TestDisconnectSKAEmissionBlock(t *testing.T) {
	placements := []struct {
		name      string
		placement emitPlacement
	}{
		{"EmissionAlone", emitAlone},
		{"EmissionBeforeSpend", emitBeforeSpend},
		{"EmissionAfterSpend", emitAfterSpend},
	}
	for _, p := range placements {
		t.Run(p.name, func(t *testing.T) {
			t.Run("Disapproval", func(t *testing.T) {
				h := newSKAEmissionHarness(t, p.placement)

				//   ... -> bemit -> bdis (disapproves bemit)
				outs := h.OldestCoinbaseOuts()
				h.NextBlock("bdis", nil, outs[1:], func(b *wire.MsgBlock) {
					b.Header.VoteBits &^= 0x0001
					h.ReplaceVoteBits(0x0000)(b)
				})
				h.AssertTipDisapprovesPrevious()
				h.AcceptTipBlock()

				h.assertSKAEmissionUndone()
				h.flushUtxoCache()
				h.assertSKAEmissionUndone()
			})

			t.Run("Reorg", func(t *testing.T) {
				h := newSKAEmissionHarness(t, p.placement)

				//   ... -> bemit
				//      \-> bx2 -> by2
				h.SetTip(h.preEmit)
				h.NextBlock("bx2", nil, nil)
				h.AcceptedToSideChainWithExpectedTip("bemit")
				h.NextBlock("by2", nil, nil)
				h.AcceptTipBlock()

				h.assertSKAEmissionUndone()
				h.flushUtxoCache()
				h.assertSKAEmissionUndone()
			})
		})
	}
}

// TestDisconnectSKAEmissionInputs ensures disconnecting the regular tree of a
// block restores the inputs of its other transactions from the spend journal
// wherever the SKA emission sits in the tree, since the emission's null input
// has no spend journal entry.
func TestDisconnectSKAEmissionInputs(t *testing.T) {
	pkScript := hexToBytes("76a9146edbc6c4d31bae9f1ccc38538a114bf42de65e8688ac")
	coinbase := wire.NewMsgTx()
	coinbase.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: wire.MaxPrevOutIndex},
		SignatureScript:  []byte{0x00, 0x00},
	})
	coinbase.AddTxOut(&wire.TxOut{Value: 1, PkScript: pkScript})
	emission := wire.NewMsgTx()
	emission.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: wire.MaxPrevOutIndex},
		SignatureScript:  []byte{0x01, 'S', 'K', 'A', 0x03},
	})
	emission.AddTxOut(&wire.TxOut{CoinType: 1, SKAValue: big.NewInt(1e18),
		PkScript: pkScript})
	prevOut := wire.OutPoint{Hash: chainhash.Hash{0xaa}}
	spend := wire.NewMsgTx()
	spend.AddTxIn(&wire.TxIn{PreviousOutPoint: prevOut, ValueIn: 5e8})
	spend.AddTxOut(&wire.TxOut{Value: 5e8 - 1000, PkScript: pkScript})
	spendStxo := spentTxOut{amount: 5e8, pkScript: pkScript, blockHeight: 10,
		blockIndex: 1, coinType: cointype.CoinTypeVAR}
	stakeStxo := spentTxOut{amount: 7e8, pkScript: []byte{0xba, 0x76},
		blockHeight: 3, coinType: cointype.CoinTypeVAR}

	tests := []struct {
		name  string
		txns  []*wire.MsgTx
		stxos []spentTxOut // stake tree entries first, as in the journal
	}{{
		name:  "emission after spend",
		txns:  []*wire.MsgTx{coinbase, spend, emission},
		stxos: []spentTxOut{spendStxo},
	}, {
		name:  "emission before spend with stake spends",
		txns:  []*wire.MsgTx{coinbase, emission, spend},
		stxos: []spentTxOut{stakeStxo, spendStxo},
	}}
	for _, test := range tests {
		// The journal only holds the regular tree entry, so decode it on its
		// own against the regular transactions.
		serialized, err := serializeSpendJournalEntry(test.stxos[len(test.stxos)-1:])
		if err != nil {
			t.Fatalf("%s: serialize: %v", test.name, err)
		}
		got, err := deserializeSpendJournalEntry(serialized, test.txns[1:])
		if err != nil {
			t.Fatalf("%s: deserialize: %v", test.name, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: decoded %d stxos, want 1", test.name, len(got))
		}

		block := dcrutil.NewBlock(&wire.MsgBlock{
			Header:       wire.BlockHeader{Height: 20},
			Transactions: test.txns,
		})
		view := NewUtxoViewpoint(nil)
		err = view.disconnectRegularTransactions(block, test.stxos, false)
		if err != nil {
			t.Fatalf("%s: disconnect: %v", test.name, err)
		}
		entry := view.entries[prevOut]
		if entry == nil || entry.IsSpent() || entry.Amount() != 5e8 {
			t.Fatalf("%s: spent input not restored: %+v", test.name, entry)
		}
		null := wire.OutPoint{Index: wire.MaxPrevOutIndex}
		if entry := view.entries[null]; entry != nil {
			t.Fatalf("%s: entry created for the emission's null input",
				test.name)
		}
	}
}
