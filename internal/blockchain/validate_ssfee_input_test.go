// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"bytes"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/monetarium/monetarium-node/blockchain/stake"
	"github.com/monetarium/monetarium-node/blockchain/standalone"
	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/chaincfg/chainhash"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/database"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/txscript"
	"github.com/monetarium/monetarium-node/wire"
)

// ssFeeTestOutput describes an existing SSFee output in a test UTXO view.
type ssFeeTestOutput struct {
	outpoint wire.OutPoint
	entry    *UtxoEntry
}

// newSSFeeTestOutput returns an unspent output of an SSFee transaction mined at
// the given height that pays value of the given coin type to pkScript.
func newSSFeeTestOutput(coinType cointype.CoinType, value int64, pkScript []byte,
	height int64) ssFeeTestOutput {

	entry := &UtxoEntry{
		coinType:      coinType,
		pkScript:      pkScript,
		blockHeight:   uint32(height),
		blockIndex:    3,
		scriptVersion: 0,
		packedFlags:   encodeUtxoFlags(false, false, stake.TxTypeSSFee),
	}
	if coinType.IsSKA() {
		entry.skaAmount = big.NewInt(value)
	} else {
		entry.amount = value
	}
	return ssFeeTestOutput{
		outpoint: wire.OutPoint{Hash: chainhash.Hash{0x5f}, Index: 1,
			Tree: wire.TxTreeStake},
		entry: entry,
	}
}

// view returns a UTXO view that contains only the output.
func (o ssFeeTestOutput) view() *UtxoViewpoint {
	view := NewUtxoViewpoint(nil)
	view.Entries()[o.outpoint] = o.entry
	return view
}

// spendingTxIn returns an input spending the output with a correct fraud proof.
func (o ssFeeTestOutput) spendingTxIn() *wire.TxIn {
	txIn := &wire.TxIn{
		PreviousOutPoint: o.outpoint,
		Sequence:         wire.MaxTxInSequenceNum,
		BlockHeight:      uint32(o.entry.BlockHeight()),
		BlockIndex:       o.entry.BlockIndex(),
	}
	if o.entry.CoinType().IsSKA() {
		txIn.SKAValueIn = o.entry.SKAAmount()
	} else {
		txIn.ValueIn = o.entry.Amount()
	}
	return txIn
}

// newTestTxOut returns an output paying value of the given coin type to
// pkScript.
func newTestTxOut(coinType cointype.CoinType, value int64, pkScript []byte) *wire.TxOut {
	txOut := &wire.TxOut{CoinType: coinType, PkScript: pkScript}
	if coinType.IsSKA() {
		txOut.SKAValue = big.NewInt(value)
	} else {
		txOut.Value = value
	}
	return txOut
}

// newSSFeeTestTx returns an SSFee transaction with the given input that carries
// the marker and pays value to pkScript, laid out as the miner lays it out.
func newSSFeeTestTx(txIn *wire.TxIn, coinType cointype.CoinType, marker []byte,
	value int64, pkScript []byte) *wire.MsgTx {

	tx := wire.NewMsgTx()
	tx.Version = wire.TxVersionTreasury
	tx.AddTxIn(txIn)
	tx.AddTxOut(newTestTxOut(coinType, 0, marker))
	tx.AddTxOut(newTestTxOut(coinType, value, pkScript))
	return tx
}

// TestCheckTransactionInputsAugmentedSSFee ensures CheckTransactionInputs
// accepts an SSFee transaction that spends an earlier SSFee output and pays
// back to its script, including right after that output is created.
func TestCheckTransactionInputsAugmentedSSFee(t *testing.T) {
	params := chaincfg.SimNetParams()
	subsidyCache := standalone.NewSubsidyCache(params)
	const originHeight = 1000
	const ska1 = cointype.CoinType(1)
	if !params.IsSKACoinTypeActive(ska1) {
		t.Fatalf("test requires %v to be active on simnet", ska1)
	}

	stakerScript, err := stake.ConsolidationAddrToPkScript(bytes.Repeat([]byte{0x11}, 20))
	if err != nil {
		t.Fatal(err)
	}
	minerScript, err := stake.ConsolidationAddrToPkScript(bytes.Repeat([]byte{0x22}, 20))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		coinType cointype.CoinType
		marker   []byte
		pkScript []byte
	}{{
		name:     "VAR staker",
		coinType: cointype.CoinTypeVAR,
		marker:   stake.CreateStakerSSFeeMarker(originHeight+1, 0),
		pkScript: stakerScript,
	}, {
		name:     "SKA staker",
		coinType: ska1,
		marker:   stake.CreateStakerSSFeeMarker(originHeight+1, 0),
		pkScript: stakerScript,
	}, {
		name:     "SKA miner",
		coinType: ska1,
		marker:   stake.CreateMinerSSFeeMarker(originHeight + 1),
		pkScript: minerScript,
	}}

	// The maturity of OP_SSGEN tagged outputs is CoinbaseMaturity once the
	// treasury agenda is active, so the next block only passes through the
	// exemption for an SSFee spending an SSFee output.
	if params.CoinbaseMaturity <= 1 {
		t.Fatalf("test requires CoinbaseMaturity > 1, got %d",
			params.CoinbaseMaturity)
	}

	for _, test := range tests {
		prev := newSSFeeTestOutput(test.coinType, 5000, test.pkScript,
			originHeight)
		tx := newSSFeeTestTx(prev.spendingTxIn(), test.coinType, test.marker,
			5000+700, test.pkScript)
		if !stake.IsSSFee(tx) {
			t.Fatalf("%s: test transaction is not an SSFee", test.name)
		}

		fee, err := CheckTransactionInputs(subsidyCache, dcrutil.NewTx(tx),
			originHeight+1, prev.view(), true, params, nil, true, false,
			standalone.SSVMonetarium)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", test.name, err)
			continue
		}
		if fee.Sign() != 0 {
			t.Errorf("%s: fee %v, want 0", test.name, fee)
		}
	}
}

// TestCheckTransactionInputsRejectsInvalidAugmentedSSFee ensures
// CheckTransactionInputs rejects an SSFee transaction whose real input does not
// spend an existing SSFee output with a matching fraud proof, or which does not
// pay back to the script of that output.
func TestCheckTransactionInputsRejectsInvalidAugmentedSSFee(t *testing.T) {
	params := chaincfg.SimNetParams()
	subsidyCache := standalone.NewSubsidyCache(params)
	const originHeight = 1000
	const ska1 = cointype.CoinType(1)

	ssFeeScript, err := stake.ConsolidationAddrToPkScript(bytes.Repeat([]byte{0x11}, 20))
	if err != nil {
		t.Fatal(err)
	}
	otherScript, err := stake.ConsolidationAddrToPkScript(bytes.Repeat([]byte{0x22}, 20))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*ssFeeTestOutput, *wire.MsgTx)
		want   ErrorKind
	}{
		{
			name: "spends a missing output",
			mutate: func(_ *ssFeeTestOutput, tx *wire.MsgTx) {
				tx.TxIn[0].PreviousOutPoint.Index++
			},
			want: ErrMissingTxOut,
		},
		{
			name: "spends a spent output",
			mutate: func(prev *ssFeeTestOutput, _ *wire.MsgTx) {
				prev.entry.Spend()
			},
			want: ErrMissingTxOut,
		},
		{
			// An untagged output has no maturity rule, so only the check of
			// the transaction that created it rejects the spend.
			name: "spends a non-SSFee output",
			mutate: func(prev *ssFeeTestOutput, tx *wire.MsgTx) {
				prev.entry.packedFlags = encodeUtxoFlags(false, false,
					stake.TxTypeRegular)
				prev.entry.pkScript = ssFeeScript[1:]
				tx.TxOut[1].PkScript = ssFeeScript[1:]
			},
			want: ErrBadSSFeeInput,
		},
		{
			// The maturity exemption covers only OP_SSGEN tagged outputs,
			// and a miner SSFee may pay to any script.
			name: "spends an immature OP_SSRTX tagged SSFee output",
			mutate: func(prev *ssFeeTestOutput, tx *wire.MsgTx) {
				script := append([]byte{txscript.OP_SSRTX}, ssFeeScript[1:]...)
				prev.entry.pkScript = script
				tx.TxOut[1].PkScript = script
			},
			want: ErrImmatureSpend,
		},
		{
			// The output pays to the same script, so only the coin type
			// check rejects the spend.
			name: "SKA SSFee spends a VAR SSFee output",
			mutate: func(_ *ssFeeTestOutput, tx *wire.MsgTx) {
				for i, txOut := range tx.TxOut {
					tx.TxOut[i] = newTestTxOut(ska1, txOut.Value,
						txOut.PkScript)
				}
			},
			want: ErrInputCoinTypeMismatch,
		},
		{
			name: "VAR SSFee spends an SKA SSFee output",
			mutate: func(prev *ssFeeTestOutput, tx *wire.MsgTx) {
				prev.entry.coinType = ska1
				prev.entry.skaAmount = big.NewInt(prev.entry.amount)
				prev.entry.amount = 0
				*tx.TxIn[0] = *prev.spendingTxIn()
			},
			want: ErrInputCoinTypeMismatch,
		},
		{
			name: "fraud proof with a wrong block height",
			mutate: func(_ *ssFeeTestOutput, tx *wire.MsgTx) {
				tx.TxIn[0].BlockHeight++
			},
			want: ErrFraudBlockHeight,
		},
		{
			name: "fraud proof with a wrong block index",
			mutate: func(_ *ssFeeTestOutput, tx *wire.MsgTx) {
				tx.TxIn[0].BlockIndex++
			},
			want: ErrFraudBlockIndex,
		},
		{
			name: "has no payment output",
			mutate: func(_ *ssFeeTestOutput, tx *wire.MsgTx) {
				tx.TxOut = tx.TxOut[:1]
			},
			want: ErrBadSSFeeInput,
		},
		{
			name: "pays to a different script",
			mutate: func(_ *ssFeeTestOutput, tx *wire.MsgTx) {
				tx.TxOut[1].PkScript = otherScript
			},
			want: ErrBadSSFeeInput,
		},
		{
			name: "pays with a different script version",
			mutate: func(_ *ssFeeTestOutput, tx *wire.MsgTx) {
				tx.TxOut[1].Version = 1
			},
			want: ErrBadSSFeeInput,
		},
		{
			name: "second payment output pays to a different script",
			mutate: func(_ *ssFeeTestOutput, tx *wire.MsgTx) {
				tx.AddTxOut(newTestTxOut(cointype.CoinTypeVAR, 100, otherScript))
			},
			want: ErrBadSSFeeInput,
		},
		{
			name: "second payment output pays to an empty script",
			mutate: func(_ *ssFeeTestOutput, tx *wire.MsgTx) {
				tx.AddTxOut(newTestTxOut(cointype.CoinTypeVAR, 100, nil))
			},
			want: ErrBadSSFeeInput,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prev := newSSFeeTestOutput(cointype.CoinTypeVAR, 5000,
				ssFeeScript, originHeight)
			tx := newSSFeeTestTx(prev.spendingTxIn(), cointype.CoinTypeVAR,
				stake.CreateStakerSSFeeMarker(originHeight+1, 0),
				5700, ssFeeScript)
			test.mutate(&prev, tx)

			_, err := CheckTransactionInputs(subsidyCache, dcrutil.NewTx(tx),
				originHeight+1, prev.view(), true, params, nil, true, false,
				standalone.SSVMonetarium)
			if !errors.Is(err, test.want) {
				t.Fatalf("got error %v, want %v", err, test.want)
			}
		})
	}
}

// TestCheckTransactionInputsSpendMatureSSFeeOutput ensures CheckTransactionInputs
// accepts a regular transaction that spends an SSFee output once the output has
// reached the maturity required of OP_SSGEN tagged outputs.
func TestCheckTransactionInputsSpendMatureSSFeeOutput(t *testing.T) {
	params := chaincfg.SimNetParams()
	subsidyCache := standalone.NewSubsidyCache(params)
	const originHeight = 1000
	const ska1 = cointype.CoinType(1)

	ssFeeScript, err := stake.ConsolidationAddrToPkScript(bytes.Repeat([]byte{0x11}, 20))
	if err != nil {
		t.Fatal(err)
	}
	payScript := ssFeeScript[1:] // the same P2PKH without the OP_SSGEN tag

	for _, coinType := range []cointype.CoinType{cointype.CoinTypeVAR, ska1} {
		for _, isTreasuryEnabled := range []bool{false, true} {
			maturity := int64(params.SStxChangeMaturity)
			if isTreasuryEnabled {
				maturity = int64(params.CoinbaseMaturity)
			}

			prev := newSSFeeTestOutput(coinType, 5000, ssFeeScript, originHeight)
			tx := wire.NewMsgTx()
			tx.AddTxIn(prev.spendingTxIn())
			tx.AddTxOut(newTestTxOut(coinType, 4000, payScript))

			fee, err := CheckTransactionInputs(subsidyCache, dcrutil.NewTx(tx),
				originHeight+maturity, prev.view(), true, params, nil,
				isTreasuryEnabled, false, standalone.SSVMonetarium)
			if err != nil {
				t.Errorf("%v (treasury %v): unexpected error: %v", coinType,
					isTreasuryEnabled, err)
				continue
			}
			if fee.Cmp(big.NewInt(1000)) != 0 {
				t.Errorf("%v (treasury %v): fee %v, want 1000", coinType,
					isTreasuryEnabled, fee)
			}
		}
	}
}

// TestCheckTransactionInputsRejectsImmatureRegularSSFeeSpend ensures the
// maturity exemption for SSFee outputs does not extend to a regular transaction
// that spends one.
func TestCheckTransactionInputsRejectsImmatureRegularSSFeeSpend(t *testing.T) {
	params := chaincfg.SimNetParams()
	subsidyCache := standalone.NewSubsidyCache(params)
	const originHeight = 1000

	ssFeeScript, err := stake.ConsolidationAddrToPkScript(bytes.Repeat([]byte{0x11}, 20))
	if err != nil {
		t.Fatal(err)
	}
	prev := newSSFeeTestOutput(cointype.CoinTypeVAR, 5000, ssFeeScript,
		originHeight)
	tx := wire.NewMsgTx()
	tx.AddTxIn(prev.spendingTxIn())
	tx.AddTxOut(newTestTxOut(cointype.CoinTypeVAR, 4000, ssFeeScript[1:]))

	_, err = CheckTransactionInputs(subsidyCache, dcrutil.NewTx(tx),
		originHeight+1, prev.view(), true, params, nil, true, false,
		standalone.SSVMonetarium)
	if !errors.Is(err, ErrImmatureSpend) {
		t.Fatalf("got error %v, want ErrImmatureSpend", err)
	}
}

// TestCheckTransactionInputsNullInputSSFee ensures CheckTransactionInputs still
// accepts SSFee transactions with a null input without consulting the UTXO
// view, including ones whose null input carries a non-zero value.
func TestCheckTransactionInputsNullInputSSFee(t *testing.T) {
	params := chaincfg.SimNetParams()
	subsidyCache := standalone.NewSubsidyCache(params)
	const height = 1000
	const ska1 = cointype.CoinType(1)

	pkScript, err := stake.ConsolidationAddrToPkScript(bytes.Repeat([]byte{0x11}, 20))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		coinType cointype.CoinType
		marker   []byte
		valueIn  int64
	}{{
		name:     "VAR staker",
		coinType: cointype.CoinTypeVAR,
		marker:   stake.CreateStakerSSFeeMarker(height, 0),
	}, {
		name:     "SKA staker",
		coinType: ska1,
		marker:   stake.CreateStakerSSFeeMarker(height, 0),
	}, {
		name:     "SKA miner with non-zero SKAValueIn",
		coinType: ska1,
		marker:   stake.CreateMinerSSFeeMarker(height),
		valueIn:  700,
	}, {
		name:     "SKA staker with non-zero SKAValueIn",
		coinType: ska1,
		marker:   stake.CreateStakerSSFeeMarker(height, 0),
		valueIn:  700,
	}}

	for _, test := range tests {
		tx := newNullInputSSFeeTestTx(test.coinType, test.marker, 700, pkScript)
		if test.coinType.IsSKA() {
			tx.TxIn[0].SKAValueIn = big.NewInt(test.valueIn)
		} else {
			tx.TxIn[0].ValueIn = test.valueIn
		}
		if !stake.IsSSFee(tx) {
			t.Fatalf("%s: test transaction is not an SSFee", test.name)
		}

		fee, err := CheckTransactionInputs(subsidyCache, dcrutil.NewTx(tx),
			height, NewUtxoViewpoint(nil), true, params, nil, true, false,
			standalone.SSVMonetarium)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", test.name, err)
			continue
		}
		if fee.Sign() != 0 {
			t.Errorf("%s: fee %v, want 0", test.name, fee)
		}
	}
}

// newNullInputSSFeeTestTx returns a null-input SSFee transaction that carries
// the marker and pays value to pkScript.
func newNullInputSSFeeTestTx(coinType cointype.CoinType, marker []byte,
	value int64, pkScript []byte) *wire.MsgTx {

	txIn := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: wire.MaxPrevOutIndex},
		Sequence:         wire.MaxTxInSequenceNum,
		BlockHeight:      wire.NullBlockHeight,
		BlockIndex:       wire.NullBlockIndex,
	}
	return newSSFeeTestTx(txIn, coinType, marker, value, pkScript)
}

// augmentingSSFeeTxIn returns the input of an SSFee transaction spending the
// payment output of the SSFee transaction at the given position in the stake
// tree of the block at the given height.
func augmentingSSFeeTxIn(src *wire.MsgTx, height int64, index uint32) *wire.TxIn {
	const paymentIdx = 1 // the layout of newSSFeeTestTx
	txIn := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: src.TxHash(), Index: paymentIdx,
			Tree: wire.TxTreeStake},
		Sequence:    wire.MaxTxInSequenceNum,
		BlockHeight: uint32(height),
		BlockIndex:  index,
	}
	if src.TxOut[paymentIdx].CoinType.IsSKA() {
		txIn.SKAValueIn = new(big.Int).Set(src.TxOut[paymentIdx].SKAValue)
	} else {
		txIn.ValueIn = src.TxOut[paymentIdx].Value
	}
	return txIn
}

// TestValidateSSFeeTxnsRejectsMismatchedType ensures validateSSFeeTxns rejects
// an SSFee transaction that augments an output of an SSFee transaction of the
// other type, in both directions.
func TestValidateSSFeeTxnsRejectsMismatchedType(t *testing.T) {
	params := chaincfg.SimNetParams()
	b := newFakeChain(params)
	const ska1 = cointype.CoinType(1)

	hash160 := bytes.Repeat([]byte{0x11}, 20)
	payScript, err := stake.ConsolidationAddrToPkScript(hash160)
	if err != nil {
		t.Fatal(err)
	}

	// The parent block holds a staker and a miner SSFee.
	srcSF := newNullInputSSFeeTestTx(ska1, stake.CreateStakerSSFeeMarker(1, 0),
		5000, payScript)
	srcMF := newNullInputSSFeeTestTx(ska1, stake.CreateMinerSSFeeMarker(1),
		3000, payScript)
	srcBlock := &wire.MsgBlock{
		Header:        wire.BlockHeader{PrevBlock: b.bestChain.Tip().hash, Height: 1},
		STransactions: []*wire.MsgTx{srcSF, srcMF},
	}
	srcNode := newBlockNode(&srcBlock.Header, b.bestChain.Tip())
	b.recentBlocks.Put(srcNode.hash, dcrutil.NewBlock(srcBlock))

	vote := wire.NewMsgTx()
	vote.AddTxOut(&wire.TxOut{PkScript: []byte{txscript.OP_RETURN}})
	vote.AddTxOut(&wire.TxOut{PkScript: []byte{txscript.OP_RETURN}})
	vote.AddTxOut(&wire.TxOut{Value: 100, PkScript: payScript[1:]})
	consolidation, err := stake.CreateSSFeeConsolidationOutput(hash160)
	if err != nil {
		t.Fatal(err)
	}
	vote.AddTxOut(consolidation)

	totalFees := wire.NewFeesByType()
	totalFees.Add(ska1, 1000)
	work, stakeShare, _, _ := standalone.GetSubsidyProportions(standalone.SSVMonetarium)
	minerFees, stakerFees := wire.CalcFeeSplitByCoinType(totalFees, work, stakeShare)
	minerFee := minerFees.GetBig(ska1).Int64()
	stakerFee := stakerFees.GetBig(ska1).Int64()

	// Each block holds one SSFee that augments an output of the other type
	// and one valid null-input SSFee.
	tests := []struct {
		name      string
		ssFeeTxns []*wire.MsgTx
	}{{
		name: "miner SSFee spends a staker SSFee output",
		ssFeeTxns: []*wire.MsgTx{
			newNullInputSSFeeTestTx(ska1, stake.CreateStakerSSFeeMarker(2, 0),
				stakerFee, payScript),
			newSSFeeTestTx(augmentingSSFeeTxIn(srcSF, 1, 0), ska1,
				stake.CreateMinerSSFeeMarker(2), 5000+minerFee, payScript),
		},
	}, {
		name: "staker SSFee spends a miner SSFee output",
		ssFeeTxns: []*wire.MsgTx{
			newSSFeeTestTx(augmentingSSFeeTxIn(srcMF, 1, 1), ska1,
				stake.CreateStakerSSFeeMarker(2, 0), 3000+stakerFee, payScript),
			newNullInputSSFeeTestTx(ska1, stake.CreateMinerSSFeeMarker(2),
				minerFee, payScript),
		},
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			msgBlock := &wire.MsgBlock{
				Header: wire.BlockHeader{PrevBlock: srcNode.hash, Height: 2},
				STransactions: append([]*wire.MsgTx{vote},
					test.ssFeeTxns...),
			}
			node := newBlockNode(&msgBlock.Header, srcNode)

			err := b.validateSSFeeTxns(dcrutil.NewBlock(msgBlock), node,
				[]*dcrutil.Tx{dcrutil.NewTx(vote)}, test.ssFeeTxns,
				totalFees, standalone.SSVMonetarium)
			if !errors.Is(err, ErrBadSSFeeInput) {
				t.Fatalf("got error %v, want ErrBadSSFeeInput", err)
			}
		})
	}
}

// TestValidateSSFeeTxnsRejectsMinerSSFeeWithoutFee ensures validateSSFeeTxns
// rejects a miner SSFee in a block without miner fees for its coin type, with a
// null input and paying nothing, or augmenting a miner SSFee output and paying
// it back unchanged.
func TestValidateSSFeeTxnsRejectsMinerSSFeeWithoutFee(t *testing.T) {
	params := chaincfg.SimNetParams()
	b := newFakeChain(params)
	const ska1 = cointype.CoinType(1)

	hash160 := bytes.Repeat([]byte{0x11}, 20)
	payScript, err := stake.ConsolidationAddrToPkScript(hash160)
	if err != nil {
		t.Fatal(err)
	}

	// The parent block holds a miner SSFee.
	srcMF := newNullInputSSFeeTestTx(ska1, stake.CreateMinerSSFeeMarker(1),
		3000, payScript)
	srcBlock := &wire.MsgBlock{
		Header:        wire.BlockHeader{PrevBlock: b.bestChain.Tip().hash, Height: 1},
		STransactions: []*wire.MsgTx{srcMF},
	}
	srcNode := newBlockNode(&srcBlock.Header, b.bestChain.Tip())
	b.recentBlocks.Put(srcNode.hash, dcrutil.NewBlock(srcBlock))

	vote := wire.NewMsgTx()
	vote.AddTxOut(&wire.TxOut{PkScript: []byte{txscript.OP_RETURN}})
	vote.AddTxOut(&wire.TxOut{PkScript: []byte{txscript.OP_RETURN}})
	vote.AddTxOut(&wire.TxOut{Value: 100, PkScript: payScript[1:]})
	consolidation, err := stake.CreateSSFeeConsolidationOutput(hash160)
	if err != nil {
		t.Fatal(err)
	}
	vote.AddTxOut(consolidation)

	tests := []struct {
		name  string
		ssFee *wire.MsgTx
	}{{
		name: "null input paying nothing",
		ssFee: newNullInputSSFeeTestTx(ska1, stake.CreateMinerSSFeeMarker(2),
			0, payScript),
	}, {
		name: "augmented input paid back unchanged",
		ssFee: newSSFeeTestTx(augmentingSSFeeTxIn(srcMF, 1, 0), ska1,
			stake.CreateMinerSSFeeMarker(2), 3000, payScript),
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			msgBlock := &wire.MsgBlock{
				Header:        wire.BlockHeader{PrevBlock: srcNode.hash, Height: 2},
				STransactions: []*wire.MsgTx{vote, test.ssFee},
			}
			node := newBlockNode(&msgBlock.Header, srcNode)

			err := b.validateSSFeeTxns(dcrutil.NewBlock(msgBlock), node,
				[]*dcrutil.Tx{dcrutil.NewTx(vote)}, []*wire.MsgTx{test.ssFee},
				wire.NewFeesByType(), standalone.SSVMonetarium)
			const wantMsg = "without miner fees in it"
			if !errors.Is(err, ErrStakeFees) ||
				!strings.Contains(err.Error(), wantMsg) {

				t.Fatalf("got error %v, want %v containing %q", err,
					ErrStakeFees, wantMsg)
			}
		})
	}
}

// TestSpentSSFeeTypeRejectsInvalidReference ensures spentSSFeeType reports an
// AssertError, not a rule error, when the fraud proof of the input does not
// locate an SSFee transaction with the spent hash.
func TestSpentSSFeeTypeRejectsInvalidReference(t *testing.T) {
	params := chaincfg.SimNetParams()
	b := newFakeChain(params)
	const ska1 = cointype.CoinType(1)

	pkScript, err := stake.ConsolidationAddrToPkScript(bytes.Repeat([]byte{0x22}, 20))
	if err != nil {
		t.Fatal(err)
	}
	src := newNullInputSSFeeTestTx(ska1, stake.CreateMinerSSFeeMarker(1),
		3000, pkScript)
	other := newNullInputSSFeeTestTx(ska1, stake.CreateStakerSSFeeMarker(1, 0),
		5000, pkScript)
	noMarker := wire.NewMsgTx()
	noMarker.AddTxOut(newTestTxOut(ska1, 0, []byte{txscript.OP_RETURN}))
	noMarker.AddTxOut(newTestTxOut(ska1, 3000, pkScript))
	srcBlock := &wire.MsgBlock{
		Header:        wire.BlockHeader{PrevBlock: b.bestChain.Tip().hash, Height: 1},
		STransactions: []*wire.MsgTx{src, other, noMarker},
	}
	srcNode := newBlockNode(&srcBlock.Header, b.bestChain.Tip())
	b.recentBlocks.Put(srcNode.hash, dcrutil.NewBlock(srcBlock))
	header := wire.BlockHeader{PrevBlock: srcNode.hash, Height: 2}
	node := newBlockNode(&header, srcNode)

	tests := []struct {
		name string
		txIn *wire.TxIn
	}{{
		name: "height of the block",
		txIn: augmentingSSFeeTxIn(src, 2, 0),
	}, {
		name: "height above the block",
		txIn: augmentingSSFeeTxIn(src, 3, 0),
	}, {
		name: "out of range stake transaction index",
		txIn: augmentingSSFeeTxIn(src, 1, 3),
	}, {
		// The SSFee at that index carries a marker, so only the hash
		// check rejects it.
		name: "wrong transaction hash",
		txIn: augmentingSSFeeTxIn(src, 1, 1),
	}, {
		name: "spent transaction has no SSFee marker",
		txIn: augmentingSSFeeTxIn(noMarker, 1, 2),
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := b.spentSSFeeType(node, test.txIn,
				make(map[int64]*dcrutil.Block))
			var assertErr AssertError
			if !errors.As(err, &assertErr) {
				t.Fatalf("got error %v, want AssertError", err)
			}
		})
	}
}

// TestSpentSSFeeTypeFetchError ensures spentSSFeeType returns the error of a
// failed block fetch as is, not as a rule error, so a database fault does not
// mark the block being connected invalid, and that it does not fetch a block it
// has already read.
func TestSpentSSFeeTypeFetchError(t *testing.T) {
	params := chaincfg.SimNetParams()
	b, err := chainSetup(t, params)
	if err != nil {
		t.Fatalf("failed to setup chain instance: %v", err)
	}
	const ska1 = cointype.CoinType(1)

	pkScript, err := stake.ConsolidationAddrToPkScript(bytes.Repeat([]byte{0x22}, 20))
	if err != nil {
		t.Fatal(err)
	}

	// The parent block is neither cached nor stored in the database.
	src := newNullInputSSFeeTestTx(ska1, stake.CreateMinerSSFeeMarker(1),
		3000, pkScript)
	srcHeader := wire.BlockHeader{PrevBlock: b.bestChain.Tip().hash, Height: 1}
	srcNode := newBlockNode(&srcHeader, b.bestChain.Tip())
	header := wire.BlockHeader{PrevBlock: srcNode.hash, Height: 2}
	node := newBlockNode(&header, srcNode)

	_, err = b.spentSSFeeType(node, augmentingSSFeeTxIn(src, 1, 0),
		make(map[int64]*dcrutil.Block))
	if !errors.Is(err, database.ErrBlockNotFound) {
		t.Fatalf("got error %v, want database.ErrBlockNotFound", err)
	}
	var rerr RuleError
	if errors.As(err, &rerr) {
		t.Fatalf("got rule error %v, want the fetch error", err)
	}

	// A block already read for the block being connected is not fetched
	// again.
	srcBlock := &wire.MsgBlock{Header: srcHeader,
		STransactions: []*wire.MsgTx{src}}
	srcBlocks := map[int64]*dcrutil.Block{1: dcrutil.NewBlock(srcBlock)}
	feeType, err := b.spentSSFeeType(node, augmentingSSFeeTxIn(src, 1, 0),
		srcBlocks)
	if err != nil || feeType != "MF" {
		t.Fatalf("got SSFee type %q and error %v, want MF", feeType, err)
	}
}

// TestCheckTransactionRejectsSSFeeExpiry ensures SSFee transactions with a null
// or a real input are rejected when they set an expiry.
func TestCheckTransactionRejectsSSFeeExpiry(t *testing.T) {
	params := chaincfg.SimNetParams()
	const originHeight = 1000

	pkScript, err := stake.ConsolidationAddrToPkScript(bytes.Repeat([]byte{0x11}, 20))
	if err != nil {
		t.Fatal(err)
	}
	prev := newSSFeeTestOutput(cointype.CoinTypeVAR, 5000, pkScript,
		originHeight)
	marker := stake.CreateStakerSSFeeMarker(originHeight+1, 0)
	tests := map[string]*wire.MsgTx{
		"null input": newNullInputSSFeeTestTx(cointype.CoinTypeVAR, marker,
			700, pkScript),
		"real input": newSSFeeTestTx(prev.spendingTxIn(), cointype.CoinTypeVAR,
			marker, 5700, pkScript),
	}
	for name, tx := range tests {
		if err := CheckTransaction(tx, params, AFNone); err != nil {
			t.Fatalf("%s: unexpected error without an expiry: %v", name, err)
		}
		tx.Expiry = originHeight + 2
		err := CheckTransaction(tx, params, AFNone)
		if !errors.Is(err, ErrSSFeeExpiry) {
			t.Fatalf("%s: got error %v, want ErrSSFeeExpiry", name, err)
		}
	}
}
