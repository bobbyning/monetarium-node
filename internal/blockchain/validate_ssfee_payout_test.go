// Copyright (c) 2026 The Monetarium developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package blockchain

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
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

// ssFeeTestScriptHashVote returns ssFeeTestVote with a P2SH ("SH")
// consolidation address instead of a P2PKH one.
func ssFeeTestScriptHashVote(t *testing.T, consolidation []byte) *dcrutil.Tx {
	t.Helper()
	vote := ssFeeTestVote(t, consolidation).MsgTx()
	out, err := stake.CreateSSFeeScriptHashConsolidationOutput(consolidation)
	if err != nil {
		t.Fatal(err)
	}
	vote.TxOut[3] = out
	return dcrutil.NewTx(vote)
}

// stakerSSFeeTestTxns returns one staker SSFee transaction per consolidation
// script of the votes, each paying an equal share per vote and the first
// script in sorted order the remainder.
func stakerSSFeeTestTxns(t *testing.T, coinType cointype.CoinType,
	votes []*dcrutil.Tx, total *big.Int) []*wire.MsgTx {

	t.Helper()
	counts := make(map[string]int64)
	for _, vote := range votes {
		script, err := stake.ExtractSSFeeConsolidationScript(vote.MsgTx())
		if err != nil {
			t.Fatal(err)
		}
		counts[hex.EncodeToString(script)]++
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
		payScript, _ := hex.DecodeString(key)
		marker := stake.CreateStakerSSFeeMarker(200, uint16(i))
		txns = append(txns, ssFeeTestTx(coinType, marker, payScript, amount))
	}
	return txns
}

// ssFeeTestLayout returns votes of the given consolidation addresses, built by
// newVote, the fees of a block that pays fee of the given coin type, and SSFee
// transactions that distribute them as the block template generator does: one
// staker SSFee per address in sorted order, then for SKA a miner SSFee paying
// minerScript.
func ssFeeTestLayout(t *testing.T, coinType cointype.CoinType,
	newVote func(*testing.T, []byte) *dcrutil.Tx, addrs [][]byte, fee int64,
	minerScript []byte) ([]*dcrutil.Tx, []*wire.MsgTx, wire.FeesByType) {

	t.Helper()
	votes := make([]*dcrutil.Tx, 0, len(addrs))
	for _, addr := range addrs {
		votes = append(votes, newVote(t, addr))
	}
	ssFeeTxns, fees := ssFeeTestTxns(t, coinType, votes, fee, minerScript)
	return votes, ssFeeTxns, fees
}

// ssFeeTestTxns returns the fees and SSFee transactions of ssFeeTestLayout for
// the given votes.
func ssFeeTestTxns(t *testing.T, coinType cointype.CoinType, votes []*dcrutil.Tx,
	fee int64, minerScript []byte) ([]*wire.MsgTx, wire.FeesByType) {

	t.Helper()
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
	return ssFeeTxns, fees
}

// TestSSFeeAcceptsTemplatePayouts ensures SSFee transactions laid out as the
// block template generator does, one payout each, distribute the fees of a
// block with one vote and with votes of several consolidation addresses, with
// null and augmented inputs and with a miner payout to a script without an
// address.  The augmented P2SH cases are a layout the generator never builds,
// since it does not augment P2SH outputs, but another miner may.
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
		scriptHash  bool // votes with P2SH ("SH") consolidation addresses
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
	}, {
		name:       "VAR fees, augmented P2SH outputs",
		coinType:   cointype.CoinTypeVAR,
		votes:      [][]byte{addrA, addrB, addrA},
		fee:        2_007,
		augmented:  true,
		scriptHash: true,
	}, {
		name:       "SKA1 fees, augmented P2SH outputs",
		coinType:   1,
		votes:      [][]byte{addrC, addrA, addrB, addrA, addrC},
		fee:        1_000_000_000_000_000_007,
		augmented:  true,
		scriptHash: true,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payScript := minerScript
			if test.minerScript != nil {
				payScript = test.minerScript
			}
			newVote := ssFeeTestVote
			if test.scriptHash {
				newVote = ssFeeTestScriptHashVote
			}
			votes, ssFeeTxns, fees := ssFeeTestLayout(t, test.coinType,
				newVote, test.votes, test.fee, payScript)

			// Make each SSFee add its fee to the output of a copy of it in
			// the parent block, as the template generator does for a P2PKH
			// script when it finds an output of an SSFee of the same type and
			// script.
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
				ssFeeTestVote, [][]byte{addrA, addrB, addrA}, fee, minerScript)
			ssFeeTxns = test.munge(ssFeeTxns)

			err := (&BlockChain{}).validateSSFeeTxns(nil, nil, votes,
				ssFeeTxns, fees, standalone.SSVMonetarium)
			if !errors.Is(err, ErrStakeFees) {
				t.Fatalf("got error %v, want %v", err, ErrStakeFees)
			}
		})
	}
}

// TestSSFeeScriptHashConsolidation ensures a staker SSFee to a voter with a
// P2SH ("SH") consolidation address pays the OP_SSGEN-tagged P2SH script of
// that address: a P2PKH and a P2SH voter of the same hash160 are paid
// separately, a unit moved between their payouts is rejected, groups are
// ordered by script for the remainder, and a payout of the right hash160
// under the other script type is rejected by the check that the recipient is
// the consolidation script of a voter.
func TestSSFeeScriptHashConsolidation(t *testing.T) {
	addr := []byte{1: 0xaa, 19: 0x01}
	lowAddr := []byte{1: 0x11, 19: 0x02}
	minerAddr := []byte{1: 0xcc, 19: 0x03}
	minerScript, err := stake.ConsolidationAddrToPkScript(minerAddr)
	if err != nil {
		t.Fatal(err)
	}
	p2pkhScript, err := stake.ConsolidationAddrToPkScript(addr)
	if err != nil {
		t.Fatal(err)
	}
	p2shScript, err := stake.ConsolidationScriptHashToPkScript(addr)
	if err != nil {
		t.Fatal(err)
	}
	p2pkhVote := ssFeeTestVote(t, addr)
	p2shVote := ssFeeTestScriptHashVote(t, addr)

	const notVoterScript = "does not match any voter consolidation script"
	tests := []struct {
		name      string
		votes     []*dcrutil.Tx
		payScript []byte // replaces the payout script of ssFeeTxns[0]
		munge     func([]*wire.MsgTx)
		wantErr   string // part of the ErrStakeFees message, "" to accept
	}{{
		name:  "P2SH voter",
		votes: []*dcrutil.Tx{p2shVote},
	}, {
		name:  "P2PKH and P2SH voters of the same hash160",
		votes: []*dcrutil.Tx{p2shVote, p2pkhVote, p2shVote},
	}, {
		// The P2PKH group sorts first and is paid in ssFeeTxns[0], the P2SH
		// group in ssFeeTxns[1].  The staker total is unchanged.
		name:  "unit moved from the P2PKH to the P2SH group of a hash160",
		votes: []*dcrutil.Tx{p2shVote, p2pkhVote, p2shVote},
		munge: func(txns []*wire.MsgTx) {
			from, to := txns[0].TxOut[1], txns[1].TxOut[1]
			if from.CoinType.IsSKA() {
				from.SKAValue = new(big.Int).Sub(from.SKAValue, big.NewInt(1))
				to.SKAValue = new(big.Int).Add(to.SKAValue, big.NewInt(1))
				return
			}
			from.Value--
			to.Value++
		},
		wantErr: "SSFee amount mismatch",
	}, {
		// Two votes leave a remainder of 1 for VAR and SKA1.  The P2SH voter
		// has the smaller hash160, but every P2PKH script sorts before every
		// P2SH script, so the P2PKH group takes the remainder.
		name: "P2SH voter with a smaller hash160 than a P2PKH voter",
		votes: []*dcrutil.Tx{ssFeeTestScriptHashVote(t, lowAddr),
			p2pkhVote},
	}, {
		name:    "P2SH payout with script version 1",
		votes:   []*dcrutil.Tx{p2shVote},
		munge:   func(txns []*wire.MsgTx) { txns[0].TxOut[1].Version = 1 },
		wantErr: "script version 1 is not 0",
	}, {
		name:      "P2SH voter paid the P2PKH script",
		votes:     []*dcrutil.Tx{p2shVote},
		payScript: p2pkhScript,
		wantErr:   notVoterScript,
	}, {
		name:      "P2PKH voter paid the P2SH script",
		votes:     []*dcrutil.Tx{p2pkhVote},
		payScript: p2shScript,
		wantErr:   notVoterScript,
	}}

	for _, coinType := range []cointype.CoinType{cointype.CoinTypeVAR, 1} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("%v/%s", coinType, test.name), func(t *testing.T) {
				fee := int64(2_007)
				if coinType.IsSKA() {
					fee = 1_000_000_000_000_000_007
				}
				ssFeeTxns, fees := ssFeeTestTxns(t, coinType, test.votes,
					fee, minerScript)
				if test.payScript != nil {
					ssFeeTxns[0].TxOut[1].PkScript = test.payScript
				}
				if test.munge != nil {
					test.munge(ssFeeTxns)
				}

				err := (&BlockChain{}).validateSSFeeTxns(nil, nil,
					test.votes, ssFeeTxns, fees, standalone.SSVMonetarium)
				if test.wantErr != "" {
					if !errors.Is(err, ErrStakeFees) ||
						!strings.Contains(err.Error(), test.wantErr) {
						t.Fatalf("got error %v, want %v containing %q", err,
							ErrStakeFees, test.wantErr)
					}
					return
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			})
		}
	}
}
