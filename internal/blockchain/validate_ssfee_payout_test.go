// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"encoding/hex"
	"errors"
	"math/big"
	"sort"
	"testing"

	"github.com/monetarium/monetarium-node/blockchain/stake"
	"github.com/monetarium/monetarium-node/blockchain/standalone"
	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/txscript"
	"github.com/monetarium/monetarium-node/wire"
)

// ssFeeTestVote returns a vote with one reward output whose consolidation
// address is the given hash160.
func ssFeeTestVote(t *testing.T, consolidation []byte) *dcrutil.Tx {
	t.Helper()
	rewardScript, err := stake.ConsolidationAddrToPkScript(consolidation)
	if err != nil {
		t.Fatal(err)
	}
	vote := wire.NewMsgTx()
	vote.AddTxOut(&wire.TxOut{PkScript: []byte{txscript.OP_RETURN}})
	vote.AddTxOut(&wire.TxOut{PkScript: []byte{txscript.OP_RETURN}})
	vote.AddTxOut(&wire.TxOut{Value: 1e8, PkScript: rewardScript})
	out, err := stake.CreateSSFeeConsolidationOutput(consolidation)
	if err != nil {
		t.Fatal(err)
	}
	vote.AddTxOut(out)
	return dcrutil.NewTx(vote)
}

// ssFeeTestTx returns a null-input SSFee transaction with the given marker that
// pays amount of the given coin type to payScript, laid out as the block
// template generator does.
func ssFeeTestTx(coinType cointype.CoinType, marker, payScript []byte,
	amount *big.Int) *wire.MsgTx {

	tx := wire.NewMsgTx()
	tx.Version = 3
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: wire.MaxPrevOutIndex},
		Sequence:         wire.MaxTxInSequenceNum,
		BlockHeight:      wire.NullBlockHeight,
		BlockIndex:       wire.NullBlockIndex,
	})
	markerOut := &wire.TxOut{CoinType: coinType, PkScript: marker}
	payOut := &wire.TxOut{CoinType: coinType, PkScript: payScript}
	if coinType.IsSKA() {
		markerOut.SKAValue = new(big.Int)
		payOut.SKAValue = amount
	} else {
		payOut.Value = amount.Int64()
	}
	tx.AddTxOut(markerOut)
	tx.AddTxOut(payOut)
	return tx
}

// stakerSSFeeTestTxns returns one staker SSFee transaction per consolidation
// address of the votes, each paying an equal share per vote and the first
// address in sorted order the remainder.
func stakerSSFeeTestTxns(t *testing.T, coinType cointype.CoinType,
	votes []*dcrutil.Tx, total *big.Int) []*wire.MsgTx {

	t.Helper()
	counts := make(map[string]int64)
	for _, vote := range votes {
		hash160, err := stake.ExtractSSFeeConsolidationAddr(vote.MsgTx())
		if err != nil {
			t.Fatal(err)
		}
		counts[hex.EncodeToString(hash160)]++
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	numVotes := big.NewInt(int64(len(votes)))
	perVote := new(big.Int).Div(total, numVotes)
	remainder := new(big.Int).Sub(total, new(big.Int).Mul(perVote, numVotes))
	txns := make([]*wire.MsgTx, 0, len(keys))
	for i, key := range keys {
		amount := new(big.Int).Mul(perVote, big.NewInt(counts[key]))
		if i == 0 {
			amount.Add(amount, remainder)
		}
		hash160, _ := hex.DecodeString(key)
		payScript, err := stake.ConsolidationAddrToPkScript(hash160)
		if err != nil {
			t.Fatal(err)
		}
		marker := stake.CreateStakerSSFeeMarker(200, uint16(i))
		txns = append(txns, ssFeeTestTx(coinType, marker, payScript, amount))
	}
	return txns
}

// ssFeeTestLayout returns votes of the given consolidation addresses, the fees
// of a block that pays fee of the given coin type, and SSFee transactions that
// distribute them as the block template generator does: one staker SSFee per
// address in sorted order, then for SKA a miner SSFee paying minerScript.
func ssFeeTestLayout(t *testing.T, coinType cointype.CoinType, addrs [][]byte,
	fee int64, minerScript []byte) ([]*dcrutil.Tx, []*wire.MsgTx, wire.FeesByType) {

	t.Helper()
	votes := make([]*dcrutil.Tx, 0, len(addrs))
	for _, addr := range addrs {
		votes = append(votes, ssFeeTestVote(t, addr))
	}
	fees := wire.NewFeesByType()
	fees.AddBig(coinType, big.NewInt(fee))
	work, stakeShare, _, _ := standalone.GetSubsidyProportions(
		standalone.SSVMonetarium)
	minerFees, stakerFees := wire.CalcFeeSplitByCoinType(fees, work, stakeShare)

	ssFeeTxns := stakerSSFeeTestTxns(t, coinType, votes,
		stakerFees.GetBig(coinType))
	if coinType.IsSKA() {
		ssFeeTxns = append(ssFeeTxns, ssFeeTestTx(coinType,
			stake.CreateMinerSSFeeMarker(200), minerScript,
			minerFees.GetBig(coinType)))
	}
	return votes, ssFeeTxns, fees
}

// TestSSFeeAcceptsTemplatePayouts ensures SSFee transactions laid out as the
// block template generator does, one payout each, distribute the fees of a
// block with one vote and with votes of several consolidation addresses, with
// null and augmented inputs and with a miner payout to a script without an
// address.
func TestSSFeeAcceptsTemplatePayouts(t *testing.T) {
	addrA := []byte{1: 0xaa, 19: 0x01}
	addrB := []byte{1: 0xbb, 19: 0x02}
	addrC := []byte{1: 0xcc, 19: 0x03}
	minerScript, err := stake.ConsolidationAddrToPkScript(addrC)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		coinType    cointype.CoinType
		votes       [][]byte
		fee         int64
		minerScript []byte
		augmented   bool
	}{{
		name:     "VAR fees, one vote",
		coinType: cointype.CoinTypeVAR,
		votes:    [][]byte{addrA},
		fee:      2_007,
	}, {
		name:     "VAR fees, five votes of three addresses",
		coinType: cointype.CoinTypeVAR,
		votes:    [][]byte{addrA, addrB, addrA, addrC, addrB},
		fee:      2_007,
	}, {
		name:     "SKA1 fees, one vote",
		coinType: 1,
		votes:    [][]byte{addrB},
		fee:      1_000_000_000_000_000_007,
	}, {
		name:     "SKA1 fees, five votes of three addresses",
		coinType: 1,
		votes:    [][]byte{addrC, addrA, addrB, addrA, addrC},
		fee:      1_000_000_000_000_000_007,
	}, {
		name:        "SKA1 fees, miner without an address",
		coinType:    1,
		votes:       [][]byte{addrA},
		fee:         1_000_000_000_000_000_007,
		minerScript: []byte{txscript.OP_SSGEN, txscript.OP_TRUE},
	}, {
		name:      "VAR fees, augmented outputs",
		coinType:  cointype.CoinTypeVAR,
		votes:     [][]byte{addrA, addrB, addrA},
		fee:       2_007,
		augmented: true,
	}, {
		name:      "SKA1 fees, augmented outputs",
		coinType:  1,
		votes:     [][]byte{addrC, addrA, addrB, addrA, addrC},
		fee:       1_000_000_000_000_000_007,
		augmented: true,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payScript := minerScript
			if test.minerScript != nil {
				payScript = test.minerScript
			}
			votes, ssFeeTxns, fees := ssFeeTestLayout(t, test.coinType,
				test.votes, test.fee, payScript)

			// Make each SSFee add its fee to the output of a copy of it in
			// the parent block, as the template generator does when it finds
			// an output of an SSFee of the same type and script.
			b, node := &BlockChain{}, (*blockNode)(nil)
			if test.augmented {
				b = newFakeChain(chaincfg.SimNetParams())
				srcBlock := &wire.MsgBlock{Header: wire.BlockHeader{
					PrevBlock: b.bestChain.Tip().hash, Height: 1}}
				for i, tx := range ssFeeTxns {
					src := tx.Copy()
					srcBlock.AddSTransaction(src)
					tx.TxIn[0] = augmentingSSFeeTxIn(src, 1, uint32(i))
					payOut, prev := tx.TxOut[1], src.TxOut[1]
					if test.coinType.IsSKA() {
						payOut.SKAValue = new(big.Int).Add(payOut.SKAValue,
							prev.SKAValue)
					} else {
						payOut.Value += prev.Value
					}
				}
				srcNode := newBlockNode(&srcBlock.Header, b.bestChain.Tip())
				b.recentBlocks.Put(srcNode.hash, dcrutil.NewBlock(srcBlock))
				header := wire.BlockHeader{PrevBlock: srcNode.hash, Height: 2}
				node = newBlockNode(&header, srcNode)
			}

			err := b.validateSSFeeTxns(nil, node, votes, ssFeeTxns, fees,
				standalone.SSVMonetarium)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// splitSSFeePayout moves one atom of the payment output of the SSFee to a
// second output with the same script.
func splitSSFeePayout(tx *wire.MsgTx) {
	payOut := tx.TxOut[1]
	second := *payOut
	if payOut.CoinType.IsSKA() {
		payOut.SKAValue = new(big.Int).Sub(payOut.SKAValue, big.NewInt(1))
		second.SKAValue = big.NewInt(1)
	} else {
		payOut.Value--
		second.Value = 1
	}
	tx.AddTxOut(&second)
}

// TestSSFeeRejectsPayoutLayouts ensures SSFee transactions that differ from an
// accepted layout only in their payment outputs are rejected: a staker or miner
// SSFee without a payment output or with two, and a staker payout to its
// recipient with a script version or a script other than the consolidation
// script of that recipient.
func TestSSFeeRejectsPayoutLayouts(t *testing.T) {
	addrA := []byte{1: 0xaa, 19: 0x01}
	addrB := []byte{1: 0xbb, 19: 0x02}
	addrC := []byte{1: 0xcc, 19: 0x03}
	minerScript, err := stake.ConsolidationAddrToPkScript(addrC)
	if err != nil {
		t.Fatal(err)
	}

	// The layout pays addrA in ssFeeTxns[0] and addrB in ssFeeTxns[1], then
	// for SKA the miner in ssFeeTxns[2].  Each SSFee has the marker at output
	// 0 and the payment at output 1.
	tests := []struct {
		name     string
		coinType cointype.CoinType
		munge    func([]*wire.MsgTx) []*wire.MsgTx
	}{{
		name:     "staker SSFee without a payment output",
		coinType: cointype.CoinTypeVAR,
		munge: func(txns []*wire.MsgTx) []*wire.MsgTx {
			txns[1].TxOut = txns[1].TxOut[:1]
			return txns
		},
	}, {
		// Without a payment output the SSFee is taken as VAR, whose miner
		// share in an SKA1-only block is zero.
		name:     "extra miner SSFee without a payment output",
		coinType: 1,
		munge: func(txns []*wire.MsgTx) []*wire.MsgTx {
			tx := ssFeeTestTx(1, stake.CreateMinerSSFeeMarker(200), nil,
				nil)
			tx.TxOut = tx.TxOut[:1]
			return append(txns, tx)
		},
	}, {
		name:     "staker SSFee with two payment outputs",
		coinType: cointype.CoinTypeVAR,
		munge: func(txns []*wire.MsgTx) []*wire.MsgTx {
			splitSSFeePayout(txns[1])
			return txns
		},
	}, {
		name:     "miner SSFee with two payment outputs",
		coinType: 1,
		munge: func(txns []*wire.MsgTx) []*wire.MsgTx {
			splitSSFeePayout(txns[2])
			return txns
		},
	}, {
		name:     "staker payout with script version 1",
		coinType: 1,
		munge: func(txns []*wire.MsgTx) []*wire.MsgTx {
			txns[1].TxOut[1].Version = 1
			return txns
		},
	}, {
		name:     "staker payout as P2PKH without the OP_SSGEN tag",
		coinType: cointype.CoinTypeVAR,
		munge: func(txns []*wire.MsgTx) []*wire.MsgTx {
			txns[1].TxOut[1].PkScript = txns[1].TxOut[1].PkScript[1:]
			return txns
		},
	}, {
		name:     "staker payout as P2SH of the recipient hash",
		coinType: cointype.CoinTypeVAR,
		munge: func(txns []*wire.MsgTx) []*wire.MsgTx {
			script := []byte{txscript.OP_HASH160, txscript.OP_DATA_20}
			script = append(script, addrB...)
			txns[1].TxOut[1].PkScript = append(script, txscript.OP_EQUAL)
			return txns
		},
	}, {
		name:     "staker payout with a tagged script ending in OP_TRUE",
		coinType: cointype.CoinTypeVAR,
		munge: func(txns []*wire.MsgTx) []*wire.MsgTx {
			payOut := txns[1].TxOut[1]
			script := append([]byte(nil), payOut.PkScript...)
			script[len(script)-1] = txscript.OP_TRUE
			payOut.PkScript = script
			return txns
		},
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fee := int64(2_007)
			if test.coinType.IsSKA() {
				fee = 1_000_000_000_000_000_007
			}
			votes, ssFeeTxns, fees := ssFeeTestLayout(t, test.coinType,
				[][]byte{addrA, addrB, addrA}, fee, minerScript)
			ssFeeTxns = test.munge(ssFeeTxns)

			err := (&BlockChain{}).validateSSFeeTxns(nil, nil, votes,
				ssFeeTxns, fees, standalone.SSVMonetarium)
			if !errors.Is(err, ErrStakeFees) {
				t.Fatalf("got error %v, want %v", err, ErrStakeFees)
			}
		})
	}
}
