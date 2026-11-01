// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/monetarium/monetarium-node/blockchain/chaingen"
	"github.com/monetarium/monetarium-node/blockchain/stake"
	"github.com/monetarium/monetarium-node/blockchain/standalone"
	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrec/secp256k1"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/txscript"
	"github.com/monetarium/monetarium-node/wire"
)

// TestSSFeeDistributesSKAFees ensures a block with an SKA1 emission in the
// emission window and blocks whose SSFee transactions distribute the SKA1 fees
// of the block, unscaled by the number of votes, are accepted, and blocks whose
// SSFee transactions distribute any other amount, including the excess of an
// emission's declared input amount over its outputs, are rejected.
func TestSSFeeDistributesSKAFees(t *testing.T) {
	// Use the simnet SKA1 configuration on regnet, which the generator
	// supports, and emit SKA1 to the pay-to-script-hash OP_TRUE address of
	// the generator with an emission key owned by the test.
	params := chaincfg.RegNetParams()
	gen, err := chaingen.MakeGenerator(params)
	if err != nil {
		t.Fatalf("failed to create generator: %v", err)
	}
	priv, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := chaincfg.SimNetParams().SKACoins[1]
	params.SKACoins = map[cointype.CoinType]*chaincfg.SKACoinConfig{1: cfg}
	cfg.EmissionKey = priv.PubKey()
	cfg.EmissionAddresses = []string{gen.P2shOpTrueAddr().String()}
	g := newChaingenHarnessWithGen(t, &gen)
	g.AdvanceToStakeValidationHeight()
	g.AdvanceToHeight(uint32(cfg.EmissionHeight)-1, 1)

	minerScript, err := stake.ConsolidationAddrToPkScript(make([]byte, 20))
	if err != nil {
		t.Fatal(err)
	}
	// addSSFees adds to b the staker and miner SSFee transactions that
	// distribute distFee.
	addSSFees := func(b *wire.MsgBlock, distFee *big.Int) {
		fees := wire.NewFeesByType()
		fees.AddBig(1, distFee)
		work, stakeShare, _, _ := standalone.GetSubsidyProportions(
			standalone.SSVMonetarium)
		minerFees, stakerFees := wire.CalcFeeSplitByCoinType(fees, work,
			stakeShare)

		// All votes share one consolidation address, so the staker fees go
		// to a single SSFee transaction.
		var consolidation []byte
		for _, stx := range b.STransactions {
			if stake.IsSSGen(stx) {
				consolidation, err = stake.ExtractSSFeeConsolidationAddr(stx)
				if err != nil {
					t.Fatal(err)
				}
				break
			}
		}
		stakerScript, err := stake.ConsolidationAddrToPkScript(consolidation)
		if err != nil {
			t.Fatal(err)
		}
		height := int64(b.Header.Height)
		b.AddSTransaction(skaSSFeeTx(stake.CreateStakerSSFeeMarker(height, 0),
			stakerScript, stakerFees.GetBig(1)))
		b.AddSTransaction(skaSSFeeTx(stake.CreateMinerSSFeeMarker(height),
			minerScript, minerFees.GetBig(1)))
	}

	emitHeight := int64(cfg.EmissionHeight)
	emission := signedSKA1EmissionTx(t, params, priv, cfg.EmissionAddresses,
		cfg.EmissionAmounts, emitHeight)
	emission.TxIn[0].ValueIn = wire.NullValueIn
	emission.TxIn[0].BlockHeight = wire.NullBlockHeight
	emission.TxIn[0].BlockIndex = wire.NullBlockIndex

	// Create a block whose emission declares an input amount above its
	// outputs and whose SSFee transactions distribute the excess.  The
	// emission signature covers only the transaction prefix, so the input
	// amount is free, and distributing the excess would create SKA1.
	//
	//   ... -> bemitfee
	preEmit := g.TipName()
	excess := big.NewInt(1_000_000_000_000_000_000)
	overEmission := emission.Copy()
	overEmission.TxIn[0].SKAValueIn = new(big.Int).Add(
		cfg.EmissionAmounts[0], excess)
	outs := g.OldestCoinbaseOuts()
	g.NextBlock("bemitfee", nil, outs[1:], func(b *wire.MsgBlock) {
		b.AddTransaction(overEmission)
		addSSFees(b, excess)
	})
	_, err = g.chain.ProcessBlock(dcrutil.NewBlock(g.Tip()))
	const wantMsg = "miner SSFee (null input) for coin type 1 distributes"
	if !errors.Is(err, ErrStakeFees) ||
		!strings.Contains(err.Error(), wantMsg) {

		t.Fatalf("block bemitfee: got error %v, want %v containing %q", err,
			ErrStakeFees, wantMsg)
	}

	//   ... -> bemit
	g.SetTip(preEmit)
	var emitIndex uint32
	g.NextBlock("bemit", nil, outs[1:], func(b *wire.MsgBlock) {
		b.AddTransaction(emission)
		emitIndex = uint32(len(b.Transactions) - 1)
	})
	g.AcceptTipBlock()

	// The emission output matures after coinbase maturity since the
	// emission has an expiry.
	g.AdvanceToHeight(uint32(emitHeight)+uint32(params.CoinbaseMaturity), 1)

	payScript := emission.TxOut[0].PkScript
	opTrueSigScript, err := txscript.NewScriptBuilder().
		AddData([]byte{txscript.OP_TRUE}).Script()
	if err != nil {
		t.Fatal(err)
	}
	// skaFeeBlock returns a munger that adds an SKA1 transaction spending
	// prevOut, worth amount and created at prevHeight and prevIndex, that
	// pays the given fee, along with the staker and miner SSFee transactions
	// that distribute distFee.
	skaFeeBlock := func(prevOut wire.OutPoint, amount *big.Int, prevHeight,
		prevIndex uint32, fee, distFee *big.Int) func(*wire.MsgBlock) {

		return func(b *wire.MsgBlock) {
			spend := wire.NewMsgTx()
			spend.Version = 3
			spend.AddTxIn(&wire.TxIn{
				PreviousOutPoint: prevOut,
				Sequence:         wire.MaxTxInSequenceNum,
				SKAValueIn:       amount,
				BlockHeight:      prevHeight,
				BlockIndex:       prevIndex,
				SignatureScript:  opTrueSigScript,
			})
			spend.AddTxOut(&wire.TxOut{CoinType: 1, PkScript: payScript,
				SKAValue: new(big.Int).Sub(amount, fee)})
			b.AddTransaction(spend)
			addSSFees(b, distFee)
		}
	}

	// Spend the emission output with all votes.
	//
	//   ... -> bemit -> ... -> bska5
	fee := big.NewInt(1_000_000_000_000_001)
	emitOut := wire.OutPoint{Hash: emission.TxHash(), Index: 0}
	emitAmount := cfg.EmissionAmounts[0]
	outs = g.OldestCoinbaseOuts()
	g.NextBlock("bska5", nil, outs[1:], skaFeeBlock(emitOut, emitAmount,
		uint32(emitHeight), emitIndex, fee, fee))
	g.AcceptTipBlock()

	// spendChange returns a munger that spends the change of bska5 with
	// fewer votes and distributes distFee.
	tip := g.Tip()
	change := tip.Transactions[len(tip.Transactions)-1]
	changeOut := wire.OutPoint{Hash: change.TxHash(), Index: 0}
	spendChange := func(distFee *big.Int) func(*wire.MsgBlock) {
		return skaFeeBlock(changeOut, change.TxOut[0].SKAValue,
			tip.Header.Height, uint32(len(tip.Transactions)-1), fee, distFee)
	}

	// Create blocks that distribute the SKA1 fee scaled by the number of
	// votes, as VAR fees are, and more than the fee, blocks without the miner
	// SSFee transaction and without either SSFee transaction, and blocks whose
	// miner or staker SSFee transaction alone pays one atom more.
	//
	//   ... -> bska5 -> bscaled
	//              \-> bover
	//              \-> bnominer
	//              \-> bnossfee
	//              \-> bminerover
	//              \-> bstakerover
	const votes = 4
	tests := []struct {
		name    string
		distFee *big.Int
		munger  func(*wire.MsgBlock)
	}{
		{name: "bscaled", distFee: new(big.Int).Div(
			new(big.Int).Mul(fee, big.NewInt(votes)),
			big.NewInt(int64(params.TicketsPerBlock)))},
		{name: "bover", distFee: new(big.Int).Add(fee, big.NewInt(2))},
		{name: "bnominer", distFee: fee, munger: func(b *wire.MsgBlock) {
			b.STransactions = b.STransactions[:len(b.STransactions)-1]
		}},
		{name: "bnossfee", distFee: fee, munger: func(b *wire.MsgBlock) {
			b.STransactions = b.STransactions[:len(b.STransactions)-2]
		}},
		{name: "bminerover", distFee: fee, munger: func(b *wire.MsgBlock) {
			out := b.STransactions[len(b.STransactions)-1].TxOut[1]
			out.SKAValue = new(big.Int).Add(out.SKAValue, big.NewInt(1))
		}},
		{name: "bstakerover", distFee: fee, munger: func(b *wire.MsgBlock) {
			out := b.STransactions[len(b.STransactions)-2].TxOut[1]
			out.SKAValue = new(big.Int).Add(out.SKAValue, big.NewInt(1))
		}},
	}
	for _, test := range tests {
		mungers := []func(*wire.MsgBlock){g.ReplaceWithNVotes(votes),
			spendChange(test.distFee)}
		if test.munger != nil {
			mungers = append(mungers, test.munger)
		}
		g.SetTip("bska5")
		g.NextBlock(test.name, nil, nil, mungers...)
		g.RejectTipBlock(ErrStakeFees)
	}

	// Spend the change with fewer votes and distribute the fee in full.
	//
	//   ... -> bska5 -> bska4
	g.SetTip("bska5")
	g.NextBlock("bska4", nil, nil, g.ReplaceWithNVotes(votes), spendChange(fee))
	g.AcceptTipBlock()
}

// TestSSFeeRequiredForFees ensures a block without SSFee transactions is
// accepted only when it has no staker fees and no SKA miner fees to distribute.
func TestSSFeeRequiredForFees(t *testing.T) {
	addrA := []byte{1: 0xaa, 19: 0x01}
	addrB := []byte{1: 0xbb, 19: 0x02}
	votes := []*dcrutil.Tx{ssFeeTestVote(t, addrA), ssFeeTestVote(t, addrB),
		ssFeeTestVote(t, addrA)}

	tests := []struct {
		name     string
		coinType cointype.CoinType
		fee      int64
		wantErr  bool
	}{{
		name:     "no fees",
		coinType: cointype.CoinTypeVAR,
	}, {
		name:     "one-atom VAR fee, which goes to the miner",
		coinType: cointype.CoinTypeVAR,
		fee:      1,
	}, {
		name:     "VAR fees without SSFee",
		coinType: cointype.CoinTypeVAR,
		fee:      2_007,
		wantErr:  true,
	}, {
		name:     "one-atom SKA1 fee without SSFee",
		coinType: 1,
		fee:      1,
		wantErr:  true,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fees := wire.NewFeesByType()
			fees.AddBig(test.coinType, big.NewInt(test.fee))
			err := (&BlockChain{}).validateSSFeeTxns(nil, nil, votes, nil,
				fees, standalone.SSVMonetarium)
			if errors.Is(err, ErrStakeFees) != test.wantErr ||
				!test.wantErr && err != nil {

				t.Fatalf("got error %v, want %v: %v", err, ErrStakeFees,
					test.wantErr)
			}
		})
	}
}

// TestSSFeeRequiredForVARFees ensures a block whose VAR fees, scaled by its
// votes, leave the stakers a share is rejected without its staker SSFee
// transaction, and a block whose scaled fees leave them none is accepted
// without one.
func TestSSFeeRequiredForVARFees(t *testing.T) {
	g := newChaingenHarness(t, chaincfg.RegNetParams())
	g.AdvanceToStakeValidationHeight()

	// The spend pays a fee of two atoms.  With all five votes the stakers
	// get one of them, and with three votes the fee scales to one atom, which
	// goes to the miner.
	//
	//   ... -> bnossfee
	//     \-> bthreevotes
	tip := g.TipName()
	outs := g.OldestCoinbaseOuts()
	g.NextBlock("bnossfee", &outs[0], nil, func(b *wire.MsgBlock) {
		// The staker SSFee transaction is the last stake transaction.
		b.STransactions = b.STransactions[:len(b.STransactions)-1]
	})
	g.RejectTipBlock(ErrStakeFees)

	g.SetTip(tip)
	g.NextBlock("bthreevotes", &outs[0], nil, g.ReplaceWithNVotes(3))
	g.AcceptTipBlock()
}

// skaSSFeeTx returns a null-input SKA1 SSFee transaction with the given marker
// that pays amount to payScript.
func skaSSFeeTx(marker, payScript []byte, amount *big.Int) *wire.MsgTx {
	tx := wire.NewMsgTx()
	tx.Version = 3
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: wire.MaxPrevOutIndex},
		Sequence:         wire.MaxTxInSequenceNum,
		BlockHeight:      wire.NullBlockHeight,
		BlockIndex:       wire.NullBlockIndex,
	})
	tx.AddTxOut(&wire.TxOut{CoinType: cointype.CoinType(1),
		SKAValue: new(big.Int), PkScript: marker})
	tx.AddTxOut(&wire.TxOut{CoinType: cointype.CoinType(1), SKAValue: amount,
		PkScript: payScript})
	return tx
}
