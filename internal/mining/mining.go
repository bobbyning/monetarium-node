// Copyright (c) 2014-2016 The btcsuite developers
// Copyright (c) 2015-2024 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package mining

import (
	"bytes"
	"container/heap"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/monetarium/monetarium-node/blockchain/stake"
	"github.com/monetarium/monetarium-node/blockchain/standalone"
	"github.com/monetarium/monetarium-node/chaincfg"
	"github.com/monetarium/monetarium-node/chaincfg/chainhash"
	"github.com/monetarium/monetarium-node/cointype"
	"github.com/monetarium/monetarium-node/crypto/rand"
	"github.com/monetarium/monetarium-node/dcrutil"
	"github.com/monetarium/monetarium-node/gcs/blockcf2"
	"github.com/monetarium/monetarium-node/internal/blockalloc"
	"github.com/monetarium/monetarium-node/internal/blockchain"
	"github.com/monetarium/monetarium-node/internal/blockchain/indexers"
	"github.com/monetarium/monetarium-node/internal/fees"
	"github.com/monetarium/monetarium-node/txscript"
	"github.com/monetarium/monetarium-node/txscript/stdaddr"
	"github.com/monetarium/monetarium-node/txscript/stdscript"
	"github.com/monetarium/monetarium-node/wire"
)

var (
	// zeroHash is the zero value hash (all zeros).  It is defined as a
	// convenience.
	zeroHash chainhash.Hash

	// opTrueScript is a simple public key script that contains the OP_TRUE
	// opcode.  It is defined here to reduce garbage creation.
	opTrueScript = []byte{txscript.OP_TRUE}
)

// Config is a descriptor containing the mining configuration.
type Config struct {
	// Policy houses the policy (configuration parameters) which is used to control
	// the generation of block templates.
	Policy *Policy

	// TxSource represents a source of transactions to consider for inclusion in
	// new blocks.
	TxSource TxSource

	// TimeSource defines the median time source which is used to retrieve the
	// current time adjusted by the median time offset.  This is used when setting
	// the timestamp in the header of new blocks.
	TimeSource blockchain.MedianTimeSource

	// SubsidyCache defines a subsidy cache to use when calculating and validating
	// block and vote subsidies.
	SubsidyCache *standalone.SubsidyCache

	// ChainParams identifies which chain parameters should be used while
	// generating block templates.
	ChainParams *chaincfg.Params

	// MiningTimeOffset defines the number of seconds to offset the mining
	// timestamp of a block by (positive values are in the past).
	MiningTimeOffset int

	// BestSnapshot defines the function to use to access information about the
	// current best block.  The returned instance should be treated as immutable.
	BestSnapshot func() *blockchain.BestState

	// BlockByHash defines the function to use to search the internal chain block
	// stores and the database in an attempt to find the requested block and return
	// it.  This function should return blocks regardless of whether or not they
	// are part of the main chain.
	BlockByHash func(hash *chainhash.Hash) (*dcrutil.Block, error)

	// BlockByHeight defines the function to use to fetch the block at the
	// given height in the main chain.
	BlockByHeight func(height int64) (*dcrutil.Block, error)

	// CalcNextRequiredDifficulty defines the function to use to calculate the
	// required difficulty for the block after the given block based on the
	// difficulty retarget rules.
	CalcNextRequiredDifficulty func(hash *chainhash.Hash, timestamp time.Time) (uint32, error)

	// CalcStakeVersionByHash defines the function to use to calculate the expected
	// stake version for the block AFTER the provided block hash.
	CalcStakeVersionByHash func(hash *chainhash.Hash) (uint32, error)

	// CheckConnectBlockTemplate defines the function to use to fully validate that
	// connecting the passed block to either the tip of the main chain or its
	// parent does not violate any consensus rules, aside from the proof of work
	// requirement.
	CheckConnectBlockTemplate func(block *dcrutil.Block) error

	// CheckTicketExhaustion defines the function to use to ensure that extending
	// the block associated with the provided hash with a block that contains the
	// specified number of ticket purchases will not result in a chain that is
	// unrecoverable due to inevitable ticket exhaustion.  This scenario happens
	// when the number of live tickets drops below the number of tickets that is
	// needed to reach the next block at which any outstanding immature ticket
	// purchases that would provide the necessary live tickets mature.
	//
	// The hasStagedTicketsWithMempoolParent parameter indicates whether there are
	// staged SStx tickets in the mempool that depend on transactions in the main
	// mempool. When true, these tickets will become mineable after the parent
	// transactions are confirmed.
	CheckTicketExhaustion func(hash *chainhash.Hash, ticketPurchases uint8, hasStagedTicketsWithMempoolParent bool) error

	// CheckTransactionInputs defines the function to use to perform a series of
	// checks on the inputs to a transaction to ensure they are valid.
	// Returns the transaction fee as *big.Int to support SKA fees.
	CheckTransactionInputs func(tx *dcrutil.Tx, txHeight int64,
		view *blockchain.UtxoViewpoint, checkFraudProof bool,
		prevHeader *wire.BlockHeader, isTreasuryEnabled,
		isAutoRevocationsEnabled bool,
		subsidySplitVariant standalone.SubsidySplitVariant) (*big.Int, error)

	// CheckTSpendHasVotes defines the function to use to check whether the given
	// tspend has enough votes to be included in a block AFTER the specified block.
	CheckTSpendHasVotes func(prevHash chainhash.Hash, tspend *dcrutil.Tx) error

	// CountSigOps defines the function to use to count the number of signature
	// operations for all transaction input and output scripts in the provided
	// transaction.
	CountSigOps func(tx *dcrutil.Tx, isCoinBaseTx bool, isSSGen bool, isTreasuryEnabled bool) int

	// FetchUtxoEntry defines the function to use to load and return the requested
	// unspent transaction output from the point of view of the main chain tip.
	//
	// NOTE: Requesting an output for which there is no data will NOT return an
	// error.  Instead both the entry and the error will be nil.  This is done to
	// allow pruning of spent transaction outputs.  In practice this means the
	// caller must check if the returned entry is nil before invoking methods on
	// it.
	//
	// This function is safe for concurrent access however the returned entry (if
	// any) is NOT.
	FetchUtxoEntry func(outpoint wire.OutPoint) (*blockchain.UtxoEntry, error)

	// FetchUtxoView defines the function to use to fetch unspent transaction
	// output information.  The returned instance should be treated as immutable.
	FetchUtxoView func(tx *dcrutil.Tx, includeRegularTxns bool) (*blockchain.UtxoViewpoint, error)

	// FetchUtxoViewParentTemplate defines the function to use to fetch unspent
	// transaction output information from the point of view of just having
	// connected the given block, which must be a block template that connects to
	// the parent of the tip of the main chain.  In other words, the given block
	// must be a sibling of the current tip of the main chain.
	//
	// This should typically only be used by mining code when it is unable to
	// generate a template that extends the current tip due to being unable to
	// acquire the minimum required number of votes to extend it.
	//
	// The returned instance should be treated as immutable.
	FetchUtxoViewParentTemplate func(block *wire.MsgBlock) (*blockchain.UtxoViewpoint, error)

	// ForceHeadReorganization defines the function to use to force a
	// reorganization of the block chain to the block hash requested, so long as
	// it matches up with the current organization of the best chain.
	ForceHeadReorganization func(formerBest chainhash.Hash, newBest chainhash.Hash) error

	// HeaderByHash returns the block header identified by the given hash or an
	// error if it doesn't exist.  Note that this will return headers from both
	// the main chain and any side chains.
	HeaderByHash func(hash *chainhash.Hash) (wire.BlockHeader, error)

	// IsFinalizedTransaction defines the function to use to determine whether or
	// not a transaction is finalized.
	IsFinalizedTransaction func(tx *dcrutil.Tx, blockHeight int64, blockTime time.Time) bool

	// IsHeaderCommitmentsAgendaActive defines the function to use to determine
	// whether or not the header commitments agenda is active or not for the block
	// AFTER the given block.
	IsHeaderCommitmentsAgendaActive func(prevHash *chainhash.Hash) (bool, error)

	// IsTreasuryAgendaActive defines the function to use to determine if the
	// treasury agenda is active or not for the block AFTER the given block.
	IsTreasuryAgendaActive func(prevHash *chainhash.Hash) (bool, error)

	// IsAutoRevocationsAgendaActive defines the function to use to determine if
	// the automatic ticket revocations agenda is active or not for the block
	// AFTER the given block.
	IsAutoRevocationsAgendaActive func(prevHash *chainhash.Hash) (bool, error)

	// IsSubsidySplitAgendaActive defines the function to use to determine if
	// the modified subsidy split agenda is active or not for the block AFTER
	// the given block.
	IsSubsidySplitAgendaActive func(prevHash *chainhash.Hash) (bool, error)

	// IsSubsidySplitR2AgendaActive defines the function to use to determine if
	// the modified subsidy split round 2 agenda is active or not for the block
	// AFTER the given block.
	IsSubsidySplitR2AgendaActive func(prevHash *chainhash.Hash) (bool, error)

	// MaxTreasuryExpenditure defines the function to use to get the maximum amount
	// of funds that can be spent from the treasury by a set of TSpends for a block
	// that extends the given block hash.  The function should return 0 if it is
	// called on an invalid TVI.
	MaxTreasuryExpenditure func(preTVIBlock *chainhash.Hash) (int64, error)

	// NewUtxoViewpoint defines the function to use to create a new empty unspent
	// transaction output view.
	NewUtxoViewpoint func() *blockchain.UtxoViewpoint

	// TipGeneration defines the function to use to get the entire generation of
	// blocks stemming from the parent of the current tip.
	TipGeneration func() []chainhash.Hash

	// ValidateTransactionScripts defines the function to use to validate the
	// scripts for the passed transaction.
	ValidateTransactionScripts func(tx *dcrutil.Tx,
		utxoView *blockchain.UtxoViewpoint, flags txscript.ScriptFlags,
		isAutoRevocationsEnabled bool) error

	// FeeCalculator provides coin-type-specific fee estimation and validation.
	// This is used for intelligent transaction prioritization and fee optimization.
	FeeCalculator *fees.CoinTypeFeeCalculator

	// BlockSpaceAllocator manages proportional block space allocation between
	// coin types and provides transaction size tracking capabilities.
	BlockSpaceAllocator *BlockSpaceAllocator

	// SSFeeIndex provides efficient O(1) UTXO lookup by (coinType, address) for
	// SSFee consolidation. When provided, enables UTXO augmentation to reduce
	// dust UTXO accumulation. If nil, SSFee transactions create new UTXOs.
	SSFeeIndex *indexers.SSFeeIndex
}

// TxDesc is a descriptor about a transaction in a transaction source along with
// additional metadata.
type TxDesc struct {
	// Tx is the transaction associated with the entry.
	Tx *dcrutil.Tx

	// Type is the type of the transaction associated with the entry.
	Type stake.TxType

	// Added is the time when the entry was added to the source pool.
	Added time.Time

	// Height is the block height when the entry was added to the source
	// pool.
	Height int64

	// Fee is the total fee the transaction associated with the entry pays.
	// For VAR transactions, use this field.
	Fee int64

	// SKAFee is the total fee for SKA transactions, which can exceed int64.
	// If set, this takes precedence over Fee for SKA coin types.
	SKAFee *big.Int

	// TotalSigOps is the total signature operations for this transaction.
	TotalSigOps int

	// TxSize is the size of the transaction.
	TxSize int64
}

// TxAncestorStats is a descriptor that stores aggregated statistics for the
// unconfirmed ancestors of a transaction.
type TxAncestorStats struct {
	// Fees is the sum of all fees of unconfirmed ancestors.
	Fees int64

	// SizeBytes is the total size of all unconfirmed ancestors.
	SizeBytes int64

	// TotalSigOps is the total number of signature operations of all ancestors.
	TotalSigOps int

	// NumAncestors is the total number of ancestors for a given transaction.
	NumAncestors int

	// NumDescendants is the total number of descendants that have ancestor
	// statistics tracked for a given transaction.
	NumDescendants int
}

// VoteDesc is a descriptor about a vote transaction in a transaction source
// along with additional metadata.
type VoteDesc struct {
	VoteHash       chainhash.Hash
	TicketHash     chainhash.Hash
	ApprovesParent bool
}

const (
	// generatedBlockVersion is the version of the block being generated for
	// the main network.  It is defined as a constant here rather than using
	// the wire.BlockVersion constant since a change in the block version
	// will require changes to the generated block.  Using the wire constant
	// for generated block version could allow creation of invalid blocks
	// for the updated version.
	generatedBlockVersion = 10

	// generatedBlockVersionTest is the version of the block being generated
	// for networks other than the main network.
	generatedBlockVersionTest = 11

	// blockHeaderOverhead is the max number of bytes it takes to serialize
	// a block header and max possible transaction count.
	blockHeaderOverhead = wire.MaxBlockHeaderPayload + wire.MaxVarIntPayload

	// coinbaseFlags is some extra data appended to the coinbase script
	// sig.
	coinbaseFlags = "/mond/"

	// kilobyte is the size of a kilobyte.
	kilobyte = 1000
)

// containsTx is a helper function that checks to see if a list of transactions
// contains any of the TxIns of some transaction.
func containsTxIns(txs []*dcrutil.Tx, tx *dcrutil.Tx) bool {
	for _, txToCheck := range txs {
		for _, txIn := range tx.MsgTx().TxIn {
			if txIn.PreviousOutPoint.Hash.IsEqual(txToCheck.Hash()) {
				return true
			}
		}
	}

	return false
}

// blockWithNumVotes is a block with the number of votes currently present
// for that block. Just used for sorting.
type blockWithNumVotes struct {
	Hash     chainhash.Hash
	NumVotes uint16
}

// byNumberOfVotes implements sort.Interface to sort a slice of blocks by their
// number of votes.
type byNumberOfVotes []*blockWithNumVotes

// Len returns the number of elements in the slice.  It is part of the
// sort.Interface implementation.
func (b byNumberOfVotes) Len() int {
	return len(b)
}

// Swap swaps the elements at the passed indices.  It is part of the
// sort.Interface implementation.
func (b byNumberOfVotes) Swap(i, j int) {
	b[i], b[j] = b[j], b[i]
}

// Less returns whether the block with index i should sort before the block with
// index j.  It is part of the sort.Interface implementation.
func (b byNumberOfVotes) Less(i, j int) bool {
	return b[i].NumVotes < b[j].NumVotes
}

// SortParentsByVotes takes a list of block header hashes and sorts them
// by the number of votes currently available for them in the votes map of
// mempool.  It then returns all blocks that are eligible to be used (have
// at least a majority number of votes) sorted by number of votes, descending.
//
// This function is safe for concurrent access.
func SortParentsByVotes(txSource TxSource, currentTopBlock chainhash.Hash, blocks []chainhash.Hash, params *chaincfg.Params) []chainhash.Hash {
	// Return now when no blocks were provided.
	lenBlocks := len(blocks)
	if lenBlocks == 0 {
		return nil
	}

	// Fetch the vote metadata for the provided block hashes from the
	// mempool and filter out any blocks that do not have the minimum
	// required number of votes.
	minVotesRequired := (params.TicketsPerBlock / 2) + 1
	voteMetadata := txSource.VotesForBlocks(blocks)
	filtered := make([]*blockWithNumVotes, 0, lenBlocks)
	for i := range blocks {
		numVotes := uint16(len(voteMetadata[i]))
		if numVotes >= minVotesRequired {
			filtered = append(filtered, &blockWithNumVotes{
				Hash:     blocks[i],
				NumVotes: numVotes,
			})
		}
	}

	// Return now if there are no blocks with enough votes to be eligible to
	// build on top of.
	if len(filtered) == 0 {
		return nil
	}

	// Blocks with the most votes appear at the top of the list.
	sort.Sort(sort.Reverse(byNumberOfVotes(filtered)))
	sortedUsefulBlocks := make([]chainhash.Hash, 0, len(filtered))
	for _, bwnv := range filtered {
		sortedUsefulBlocks = append(sortedUsefulBlocks, bwnv.Hash)
	}

	// Make sure we don't reorganize the chain needlessly if the top block has
	// the same amount of votes as the current leader after the sort. After this
	// point, all blocks listed in sortedUsefulBlocks definitely also have the
	// minimum number of votes required.
	curVoteMetadata := txSource.VotesForBlocks([]chainhash.Hash{currentTopBlock})
	var numTopBlockVotes uint16
	if len(curVoteMetadata) > 0 {
		numTopBlockVotes = uint16(len(curVoteMetadata[0]))
	}
	if filtered[0].NumVotes == numTopBlockVotes && filtered[0].Hash !=
		currentTopBlock {

		// Attempt to find the position of the current block being built
		// from in the list.
		pos := 0
		for i, bwnv := range filtered {
			if bwnv.Hash == currentTopBlock {
				pos = i
				break
			}
		}

		// Swap the top block into the first position. We directly access
		// sortedUsefulBlocks useful blocks here with the assumption that
		// since the values were accumulated from filtered, they should be
		// in the same positions and we shouldn't be able to access anything
		// out of bounds.
		if pos != 0 {
			sortedUsefulBlocks[0], sortedUsefulBlocks[pos] =
				sortedUsefulBlocks[pos], sortedUsefulBlocks[0]
		}
	}

	return sortedUsefulBlocks
}

// BlockTemplate houses a block that has yet to be solved along with additional
// details about the fees and the number of signature operations for each
// transaction in the block.
type BlockTemplate struct {
	// Block is a block that is ready to be solved by miners.  Thus, it is
	// completely valid with the exception of satisfying the proof-of-work
	// requirement.
	Block *wire.MsgBlock

	// Fees contains the amount of fees each transaction in the generated
	// template pays in base units.  Since the first transaction is the
	// coinbase, the first entry (offset 0) will contain the negative of the
	// sum of the fees of all other transactions.
	// Uses *big.Int to support SKA fees which can exceed int64.
	Fees []*big.Int

	// SigOpCounts contains the number of signature operations each
	// transaction in the generated template performs.
	SigOpCounts []int64

	// Height is the height at which the block template connects to the main
	// chain.
	Height int64

	// ValidPayAddress indicates whether or not the template coinbase pays
	// to an address or is redeemable by anyone.  See the documentation on
	// NewBlockTemplate for details on which this can be useful to generate
	// templates without a coinbase payment address.
	ValidPayAddress bool
}

// mergeUtxoView adds all of the entries in viewB to viewA.  The result is that
// viewA will contain all of its original entries plus all of the entries
// in viewB.  It will replace any entries in viewB which also exist in viewA
// if the entry in viewA is spent.
func mergeUtxoView(viewA *blockchain.UtxoViewpoint, viewB *blockchain.UtxoViewpoint) {
	viewAEntries := viewA.Entries()
	for outpoint, entryB := range viewB.Entries() {
		if entryA, exists := viewAEntries[outpoint]; !exists ||
			entryA == nil || entryA.IsSpent() {
			viewAEntries[outpoint] = entryB
		}
	}
}

// hashExistsInList checks if a hash exists in a list of hash pointers.
func hashInSlice(h chainhash.Hash, list []chainhash.Hash) bool {
	for i := range list {
		if h == list[i] {
			return true
		}
	}

	return false
}

// txIndexFromTxList returns a transaction's index in a list, or -1 if it
// can not be found.
func txIndexFromTxList(hash chainhash.Hash, list []*dcrutil.Tx) int {
	for i, tx := range list {
		h := tx.Hash()
		if hash == *h {
			return i
		}
	}

	return -1
}

// standardCoinbaseOpReturn creates a standard OP_RETURN output to insert into
// coinbase. This function autogenerates the extranonce. The OP_RETURN pushes
// 12 bytes.
func standardCoinbaseOpReturn(height uint32) ([]byte, error) {
	extraNonce := rand.Uint64()

	enData := make([]byte, 12)
	binary.LittleEndian.PutUint32(enData[0:4], height)
	binary.LittleEndian.PutUint64(enData[4:12], extraNonce)
	extraNonceScript, err := stdscript.ProvablyPruneableScriptV0(enData)
	if err != nil {
		return nil, err
	}

	return extraNonceScript, nil
}

// standardTreasurybaseOpReturn creates a standard OP_RETURN output to insert
// into a treasurybase. This function autogenerates the extranonce. The
// OP_RETURN pushes 12 bytes.
func standardTreasurybaseOpReturn(height uint32) ([]byte, error) {
	extraNonce := rand.Uint64()

	enData := make([]byte, 12)
	binary.LittleEndian.PutUint32(enData[0:4], height)
	binary.LittleEndian.PutUint64(enData[4:12], extraNonce)
	extraNonceScript, err := stdscript.ProvablyPruneableScriptV0(enData)
	if err != nil {
		return nil, err
	}

	return extraNonceScript, nil
}

// calcBlockMerkleRoot calculates and returns a merkle root depending on the
// result of the header commitments agenda vote.  In particular, before the
// agenda is active, it returns the merkle root of the regular transaction tree.
// Once the agenda is active, it returns the combined merkle root for the
// regular and stake transaction trees in accordance with DCP0005.
func calcBlockMerkleRoot(regularTxns, stakeTxns []*wire.MsgTx, hdrCmtActive bool) chainhash.Hash {
	if !hdrCmtActive {
		merkleRoot := standalone.CalcTxTreeMerkleRoot(regularTxns)
		return merkleRoot
	}

	merkleRoot := standalone.CalcCombinedTxTreeMerkleRoot(regularTxns, stakeTxns)
	return merkleRoot
}

// calcBlockCommitmentRootV1 calculates and returns the required v1 block and
// the previous output scripts it references as inputs.
func calcBlockCommitmentRootV1(block *wire.MsgBlock, prevScripts blockcf2.PrevScripter) (chainhash.Hash, error) {
	filter, err := blockcf2.Regular(block, prevScripts)
	if err != nil {
		return chainhash.Hash{}, err
	}
	return blockchain.CalcCommitmentRootV1(filter.Hash()), nil
}

// createCoinbaseTx returns a coinbase transaction paying an appropriate subsidy
// based on the passed block height to the provided address.  When the address
// is nil, the coinbase transaction will instead be redeemable by anyone.
//
// See the comment for NewBlockTemplate for more information about why the nil
// address handling is useful.
func createCoinbaseTx(subsidyCache *standalone.SubsidyCache,
	coinbaseScript []byte, opReturnPkScript []byte, nextBlockHeight int64,
	addr stdaddr.Address, voters uint16, params *chaincfg.Params,
	isTreasuryEnabled bool,
	subsidySplitVariant standalone.SubsidySplitVariant) *dcrutil.Tx {

	// Coinbase transactions have no inputs, so previous outpoint is zero hash
	// and max index.
	coinbaseInput := &wire.TxIn{
		PreviousOutPoint: *wire.NewOutPoint(&chainhash.Hash{},
			wire.MaxPrevOutIndex, wire.TxTreeRegular),
		Sequence:        wire.MaxTxInSequenceNum,
		BlockHeight:     wire.NullBlockHeight,
		BlockIndex:      wire.NullBlockIndex,
		SignatureScript: coinbaseScript,
	}

	// Block one is a special block that might pay out tokens to a ledger.
	if nextBlockHeight == 1 && len(params.BlockOneLedger) != 0 {
		tx := wire.NewMsgTx()
		tx.Version = 1
		tx.AddTxIn(coinbaseInput)
		tx.TxIn[0].ValueIn = params.BlockOneSubsidy()

		for _, payout := range params.BlockOneLedger {
			tx.AddTxOut(&wire.TxOut{
				Value:    payout.Amount,
				CoinType: cointype.CoinTypeVAR,
				Version:  payout.ScriptVersion,
				PkScript: payout.Script,
			})
		}

		return dcrutil.NewTx(tx)
	}

	// Prior to the decentralized treasury agenda, the transaction version must
	// be 1 and there is an additional output that either pays to organization
	// associated with the treasury or a provably pruneable zero-value output
	// script when it is disabled.
	//
	// Once the decentralized treasury agenda is active, the transaction version
	// must be the new expected version and there is no treasury output since it
	// is included in the stake tree instead.
	var txVersion = uint16(1)
	var treasuryOutput *wire.TxOut
	var treasurySubsidy int64
	if !isTreasuryEnabled {
		if params.BlockTaxProportion > 0 {
			// Create the treasury output with the correct subsidy and public
			// key script for the organization associated with the treasury.
			treasurySubsidy = subsidyCache.CalcTreasurySubsidy(nextBlockHeight,
				voters, isTreasuryEnabled)
			treasuryOutput = &wire.TxOut{
				Value:    treasurySubsidy,
				PkScript: params.OrganizationPkScript,
			}
		} else {
			// Treasury disabled.
			treasuryOutput = &wire.TxOut{
				Value:    0,
				PkScript: opTrueScript,
			}
		}
	} else {
		// Set the transaction version to the new version required by the
		// decentralized treasury agenda.
		txVersion = wire.TxVersionTreasury
	}

	// Create the script to pay to the provided payment address if one was
	// specified.  Otherwise create a script that allows the coinbase to be
	// redeemable by anyone.
	workSubsidyScriptVer := uint16(0)
	workSubsidyScript := opTrueScript
	if addr != nil {
		workSubsidyScriptVer, workSubsidyScript = addr.PaymentScript()
	}

	// Create a coinbase with expected inputs and outputs.
	//
	// Inputs:
	//  - A single input with input value set to the total payout amount.
	//
	// Outputs:
	//  - Potential treasury output prior to the decentralized treasury agenda
	//  - Output that includes the block height and potential extra nonce used
	//    to ensure a unique hash
	//  - Output that pays the work subsidy to the miner
	workSubsidy := subsidyCache.CalcWorkSubsidyV3(nextBlockHeight, voters,
		subsidySplitVariant)
	tx := wire.NewMsgTx()
	tx.Version = txVersion
	tx.AddTxIn(coinbaseInput)
	tx.TxIn[0].ValueIn = workSubsidy + treasurySubsidy
	if treasuryOutput != nil {
		// Add CoinType to treasury output
		treasuryOutput.CoinType = cointype.CoinTypeVAR
		tx.AddTxOut(treasuryOutput)
	}
	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		CoinType: cointype.CoinTypeVAR,
		PkScript: opReturnPkScript,
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    workSubsidy,
		CoinType: cointype.CoinTypeVAR,
		Version:  workSubsidyScriptVer,
		PkScript: workSubsidyScript,
	})

	return dcrutil.NewTx(tx)
}

// createTreasuryBaseTx returns a treasurybase transaction paying an appropriate
// subsidy based on the passed block height to the treasury.
func createTreasuryBaseTx(subsidyCache *standalone.SubsidyCache, nextBlockHeight int64, voters uint16) (*dcrutil.Tx, error) {
	// Create provably pruneable script for the output that encodes the block
	// height used to ensure a unique overall transaction hash.  This is
	// necessary because neither the input nor the output that adds to the
	// treasury account balance are unique for a treasurybase.
	opReturnTreasury, err := standardTreasurybaseOpReturn(uint32(nextBlockHeight))
	if err != nil {
		return nil, err
	}

	// Create a treasurybase with expected inputs and outputs.
	//
	// Inputs:
	//  - A single input with input value set to the total payout amount.
	//
	// Outputs:
	//  - Treasury output that adds to the treasury account balance
	//  - Output that includes the block height to ensure a unique hash
	//
	// Note that all treasurybase transactions require TxVersionTreasury and
	// they must be in the stake transaction tree.
	const withTreasury = true
	trsySubsidy := subsidyCache.CalcTreasurySubsidy(nextBlockHeight, voters,
		withTreasury)
	tx := wire.NewMsgTx()
	tx.Version = wire.TxVersionTreasury
	tx.AddTxIn(&wire.TxIn{
		// Treasurybase transactions have no inputs, so previous outpoint
		// is zero hash and max index.
		PreviousOutPoint: *wire.NewOutPoint(&chainhash.Hash{},
			wire.MaxPrevOutIndex, wire.TxTreeRegular),
		Sequence:        wire.MaxTxInSequenceNum,
		BlockHeight:     wire.NullBlockHeight,
		BlockIndex:      wire.NullBlockIndex,
		SignatureScript: nil, // Must be nil by consensus.
	})
	tx.TxIn[0].ValueIn = trsySubsidy
	tx.AddTxOut(&wire.TxOut{
		Value:    trsySubsidy,
		CoinType: cointype.CoinTypeVAR,
		Version:  0,
		PkScript: []byte{txscript.OP_TADD},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		CoinType: cointype.CoinTypeVAR,
		PkScript: opReturnTreasury,
	})
	retTx := dcrutil.NewTx(tx)
	retTx.SetTree(wire.TxTreeStake)
	return retTx, nil
}

// createSSFeeTxBatched creates batched staker fee distribution transactions for all coin types,
// grouping voters by consolidation address to reduce UTXO fragmentation.
//
// This function extracts consolidation addresses from votes and groups voters with the same
// consolidation address together. It then creates one SSFee transaction per group instead of
// one per voter, significantly reducing dust UTXO accumulation.
//
// The function creates transactions with:
// - Version 3+ (required for coin type support)
// - Single input: either null input (creating new UTXO) or real UTXO input (augmenting existing)
// - Single OP_RETURN output with SSFee marker (height + voter sequence)
// - Single payment output to the consolidation address (with fee proportional to total stake)
// - The payment output has the specified coin type (VAR, SKA1, SKA2, etc.)
//
// UTXO Augmentation:
// If ssfeeIndex is provided, the function will look up existing UTXOs matching the consolidation
// address and coin type. If found, it augments the UTXO by using it as input and creating an output
// with value = utxo_value + total_fee. This prevents dust accumulation by consolidating fees into
// existing UTXOs. If no matching UTXO exists, falls back to null input (creates new UTXO).
//
// Parameters:
//   - coinType: The coin type for the fees (VAR=0, SKA1=1, SKA2=2, etc.)
//   - totalFee: Total fee to distribute across all voters
//   - voters: Vote transactions to extract consolidation addresses from
//   - nextBlockHeight: Height of the block being created
//   - ssfeeIndex: SSFee index for efficient UTXO lookup (nil disables augmentation)
//
// Returns:
//   - Array of batched SSFee transactions (one per unique consolidation address)
//   - Error if extraction or transaction creation fails
func createSSFeeTxBatched(coinType cointype.CoinType, totalFee *big.Int,
	voters []*dcrutil.Tx, nextBlockHeight int64,
	ssfeeIndex *indexers.SSFeeIndex,
	blockUtxos *blockchain.UtxoViewpoint,
	fetchUtxoEntry func(wire.OutPoint) (*blockchain.UtxoEntry, error),
	generator *BlkTmplGenerator) ([]*dcrutil.Tx, error) {

	if len(voters) == 0 || totalFee == nil || totalFee.Sign() <= 0 {
		return nil, nil
	}

	// consolidationGroup represents a group of voters with the same consolidation address
	type consolidationGroup struct {
		voterIndices []int  // Indices into voters array
		script       []byte // Consolidation script the group's SSFee pays
	}

	// Step 1: Extract consolidation scripts from votes and group voters
	groups := make(map[string]*consolidationGroup) // key: hex(script)

	for i, voter := range voters {
		voteTx := voter.MsgTx()

		// Extract consolidation script from vote
		script, err := stake.ExtractSSFeeConsolidationScript(voteTx)
		if err != nil {
			return nil, fmt.Errorf("failed to extract consolidation address from vote %s: %w",
				voter.Hash(), err)
		}

		// Group by consolidation script
		key := hex.EncodeToString(script)
		if group, exists := groups[key]; exists {
			group.voterIndices = append(group.voterIndices, i)
		} else {
			groups[key] = &consolidationGroup{
				voterIndices: []int{i},
				script:       script,
			}
		}
	}

	// Step 2: Create one batched SSFee transaction per group
	// Use equal-per-vote distribution (not stake-weighted)
	ssFeeTxns := make([]*dcrutil.Tx, 0, len(groups))

	// Calculate equal fee per voter using big.Int arithmetic
	numVotes := big.NewInt(int64(len(voters)))
	feePerVoter := new(big.Int).Div(totalFee, numVotes)
	distributed := new(big.Int).Mul(feePerVoter, numVotes)
	remainder := new(big.Int).Sub(totalFee, distributed)

	// Sort group keys for deterministic remainder assignment
	sortedKeys := make([]string, 0, len(groups))
	for key := range groups {
		sortedKeys = append(sortedKeys, key)
	}
	sort.Strings(sortedKeys)

	for i, key := range sortedKeys {
		group := groups[key]
		// Calculate this group's fee (equal per voter in group)
		groupFee := new(big.Int).Mul(feePerVoter, big.NewInt(int64(len(group.voterIndices))))
		// First sorted group gets remainder for deterministic results
		if i == 0 && remainder.Sign() > 0 {
			groupFee.Add(groupFee, remainder)
		}

		if groupFee.Sign() <= 0 {
			// Skip groups with zero fee (can happen due to rounding)
			continue
		}

		recipientPkScript := group.script

		// Step 3: Look up existing UTXO for augmentation (if index provided)
		var existingOutpoint *wire.OutPoint
		var existingValue *big.Int // Use *big.Int to avoid overflow for SKA
		var existingBlockHeight int64
		var existingBlockIndex uint32

		// The SSFee index files only P2PKH outputs, so a P2SH group always
		// gets a new output.
		pkHash := stdscript.ExtractStakeGenPubKeyHashV0(recipientPkScript)
		if ssfeeIndex != nil && pkHash != nil {
			// isMiner=false for staker SSFee
			outpoint, value, blockHeight, blockIndex, err := ssfeeIndex.LookupUTXO(false, coinType, pkHash)
			if err != nil {
				// Log but don't fail - fall back to null input
				log.Debugf("Failed to query SSFeeIndex for UTXO lookup: %v", err)
			} else if outpoint != nil && value != nil && value.Sign() > 0 {
				// Check if UTXO is already in-flight (used in a pending template)
				if generator != nil && generator.isSSFeeUTXOInFlight(*outpoint, nextBlockHeight) {
					log.Debugf("SSFee UTXO %v is in-flight for height %d - skipping", outpoint, nextBlockHeight)
					outpoint = nil
					value = nil
				}
			}

			if outpoint != nil && value != nil && value.Sign() > 0 {
				// CRITICAL: Verify UTXO is still available (not spent in mempool/current block)
				var entry *blockchain.UtxoEntry
				var isSpent bool

				// First check blockUtxos (for UTXOs already used in current template)
				if blockUtxos != nil {
					entry = blockUtxos.LookupEntry(*outpoint)
				}

				if entry != nil {
					// Entry found in block template view - check if spent
					isSpent = entry.IsSpent()
				} else if fetchUtxoEntry != nil {
					// Entry not in block template - fetch from chain UTXO set
					chainEntry, err := fetchUtxoEntry(*outpoint)
					if err != nil {
						log.Debugf("Failed to fetch UTXO %v from chain: %v", outpoint, err)
						isSpent = true // Treat error as spent
					} else if chainEntry == nil || chainEntry.IsSpent() {
						isSpent = true
					} else {
						entry = chainEntry
						isSpent = false
					}
				} else {
					// No way to verify - treat the UTXO as unavailable
					isSpent = true
				}

				if isSpent {
					log.Debugf("SSFee UTXO %v is spent - creating new UTXO instead", outpoint)
				} else if entry.ScriptVersion() != 0 ||
					!bytes.Equal(entry.PkScript(), recipientPkScript) {

					// Consensus only lets an augmented SSFee pay back to the
					// script of the output it spends.
					log.Debugf("SSFee UTXO %v pays to a different script - creating new UTXO instead", outpoint)
				} else if !generator.isSSFeeOutputOfType(outpoint, blockHeight, blockIndex, "SF") {
					log.Debugf("SSFee UTXO %v is not a staker SSFee output - creating new UTXO instead", outpoint)
				} else {
					// UTXO is available - use for augmentation
					existingOutpoint = outpoint
					existingValue = value
					existingBlockHeight = blockHeight
					existingBlockIndex = blockIndex
					log.Debugf("Found augmentable UTXO %v (value=%v, height=%d, index=%d) for consolidation script %x",
						outpoint, value, blockHeight, blockIndex, recipientPkScript)
				}
			} else if outpoint == nil {
				log.Debugf("No existing UTXO found in SSFeeIndex for consolidation script %x (will create new UTXO)",
					recipientPkScript)
			}
		}

		// Step 4: Create the batched SSFee transaction
		tx := wire.NewMsgTx()
		tx.Version = wire.TxVersionTreasury // Version 3+ required for coin type

		// Add input: either null input or existing UTXO
		if existingOutpoint != nil && existingValue != nil && existingValue.Sign() > 0 {
			// Augment existing UTXO
			txIn := &wire.TxIn{
				PreviousOutPoint: *existingOutpoint,
				Sequence:         wire.MaxTxInSequenceNum,
				BlockHeight:      uint32(existingBlockHeight),
				BlockIndex:       existingBlockIndex,
			}
			// SKA uses SKAValueIn (bigint), VAR uses ValueIn (int64)
			if coinType.IsSKA() {
				txIn.SKAValueIn = new(big.Int).Set(existingValue)
			} else {
				txIn.ValueIn = existingValue.Int64()
			}
			tx.AddTxIn(txIn)
		} else {
			// Create null input (new UTXO)
			tx.AddTxIn(&wire.TxIn{
				PreviousOutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{},
					Index: wire.MaxPrevOutIndex,
				},
				Sequence:    wire.MaxTxInSequenceNum,
				BlockHeight: wire.NullBlockHeight,
				BlockIndex:  wire.NullBlockIndex,
				ValueIn:     0,
			})
		}

		// Add OP_RETURN output with consolidation address (output[0] - standard SSFee format)
		// Use first voter in group to determine the sequence
		voterSeq := uint16(group.voterIndices[0])
		ssfeeMarker := stake.CreateStakerSSFeeMarker(nextBlockHeight, voterSeq)
		// OP_RETURN output: Value=0 for VAR, SKAValue=0 for SKA
		opReturnOut := &wire.TxOut{
			Version:  0,
			CoinType: coinType,
			PkScript: ssfeeMarker,
		}
		if coinType.IsSKA() {
			opReturnOut.SKAValue = new(big.Int) // Zero value for SKA OP_RETURN
		}
		tx.AddTxOut(opReturnOut)

		// Add payment output to consolidation address (output[1] - standard SSFee format)
		// Calculate output value using big.Int arithmetic (works for both VAR and SKA)
		outputValue := new(big.Int).Set(groupFee)
		if existingValue != nil && existingValue.Sign() > 0 {
			outputValue.Add(outputValue, existingValue)
		}

		txOut := &wire.TxOut{
			Version:  0,
			PkScript: recipientPkScript,
			CoinType: coinType,
		}
		if coinType.IsSKA() {
			txOut.SKAValue = outputValue
		} else {
			// VAR values fit in int64
			txOut.Value = outputValue.Int64()
		}
		tx.AddTxOut(txOut)

		retTx := dcrutil.NewTx(tx)
		retTx.SetTree(wire.TxTreeStake)

		// Mark the UTXO as in-flight if we used one for augmentation
		if existingOutpoint != nil && generator != nil {
			generator.markSSFeeUTXOInFlight(*existingOutpoint, nextBlockHeight)
		}

		ssFeeTxns = append(ssFeeTxns, retTx)
	}

	return ssFeeTxns, nil
}

// createMinerSSFeeTx creates a miner fee distribution transaction for non-VAR coins.
// These transactions distribute fees collected from non-VAR transactions to the miner.
//
// The transaction has:
// - Version 3+ (same as SSFee for stakers)
// - Single input: either null input (creating new UTXO) or real UTXO input (augmenting existing)
// - Single output to the miner address (with fee added to existing value if augmenting)
// - The output has the specified non-VAR coin type
//
// UTXO Augmentation:
// If utxoView is provided and contains a UTXO matching the miner address and coin type,
// this function will use that UTXO as input and create an output with value = utxo_value + fee.
// This prevents dust accumulation by consolidating fees into existing UTXOs.
// If no matching UTXO exists, falls back to null input (creates new UTXO).
func createMinerSSFeeTx(coinType cointype.CoinType, totalFee *big.Int,
	minerAddress stdaddr.Address, nextBlockHeight int64,
	ssfeeIndex *indexers.SSFeeIndex,
	blockUtxos *blockchain.UtxoViewpoint,
	fetchUtxoEntry func(wire.OutPoint) (*blockchain.UtxoEntry, error),
	generator *BlkTmplGenerator) (*dcrutil.Tx, error) {

	// SSFee cannot be used for VAR fees (those go through coinbase)
	if coinType == cointype.CoinTypeVAR {
		return nil, fmt.Errorf("miner SSFee cannot distribute VAR fees")
	}

	// Validate fee amount
	if totalFee == nil || totalFee.Sign() <= 0 {
		return nil, fmt.Errorf("invalid fee amount: %v", totalFee)
	}

	// Create OP_RETURN output for unique hash
	opReturnScript := stake.CreateMinerSSFeeMarker(nextBlockHeight)

	// Create payment script for the miner address or anyone-can-spend if nil
	// Use OP_SSGEN-tagged scripts like staker SSFee outputs for consistency
	var scriptVersion uint16
	var payScript []byte
	if minerAddress != nil {
		var rawScript []byte
		scriptVersion, rawScript = minerAddress.PaymentScript()
		// Convert to OP_SSGEN-tagged script (stake-tagged)
		// This ensures the output is properly recognized as a stake tree output
		payScript = make([]byte, 0, len(rawScript)+1)
		payScript = append(payScript, txscript.OP_SSGEN)
		payScript = append(payScript, rawScript...)
	} else {
		// Fallback to anyone-can-spend with OP_SSGEN tag
		scriptVersion = 0
		payScript = []byte{txscript.OP_SSGEN, txscript.OP_TRUE}
	}

	// Extract hash160 from payScript for SSFeeIndex lookup
	// This is needed to find existing miner SSFee UTXOs for consolidation
	var minerHash160 []byte
	if minerAddress != nil && len(payScript) == 26 {
		// OP_SSGEN-tagged P2PKH script format (26 bytes):
		// OP_SSGEN OP_DUP OP_HASH160 OP_DATA_20 <20 bytes hash160> OP_EQUALVERIFY OP_CHECKSIG
		minerHash160 = payScript[4:24]
	}

	// Try to find existing UTXO to augment using SSFeeIndex (prevent dust accumulation)
	var inputOutpoint *wire.OutPoint
	var augmentValue *big.Int // Use *big.Int to avoid overflow for SKA
	var existingBlockHeight int64
	var existingBlockIndex uint32

	if ssfeeIndex != nil && minerHash160 != nil {
		// isMiner=true for miner SSFee
		outpoint, value, blockHeight, blockIndex, err := ssfeeIndex.LookupUTXO(true, coinType, minerHash160)
		if err != nil {
			// Log but don't fail - fall back to null input
			log.Debugf("Failed to query SSFeeIndex for miner UTXO lookup: %v", err)
		} else if outpoint != nil && value != nil && value.Sign() > 0 {
			// Check if UTXO is already in-flight (used in a pending template)
			if generator != nil && generator.isSSFeeUTXOInFlight(*outpoint, nextBlockHeight) {
				log.Debugf("Miner SSFee UTXO %v is in-flight for height %d - skipping", outpoint, nextBlockHeight)
				outpoint = nil
				value = nil
			}
		}

		if outpoint != nil && value != nil && value.Sign() > 0 {
			// CRITICAL: Verify UTXO is still available (not spent in mempool/current block)
			var entry *blockchain.UtxoEntry
			var isSpent bool

			// First check blockUtxos (for UTXOs already used in current template)
			if blockUtxos != nil {
				entry = blockUtxos.LookupEntry(*outpoint)
			}

			if entry != nil {
				// Entry found in block template view - check if spent
				isSpent = entry.IsSpent()
			} else if fetchUtxoEntry != nil {
				// Entry not in block template - fetch from chain UTXO set
				chainEntry, err := fetchUtxoEntry(*outpoint)
				if err != nil {
					log.Debugf("Failed to fetch miner UTXO %v from chain: %v", outpoint, err)
					isSpent = true
				} else if chainEntry == nil || chainEntry.IsSpent() {
					isSpent = true
				} else {
					entry = chainEntry
					isSpent = false
				}
			} else {
				// No way to verify - treat the UTXO as unavailable
				isSpent = true
			}

			if isSpent {
				log.Debugf("Miner SSFee UTXO %v is spent - creating new UTXO instead", outpoint)
			} else if entry.ScriptVersion() != scriptVersion ||
				!bytes.Equal(entry.PkScript(), payScript) {

				// Consensus only lets an augmented SSFee pay back to the
				// script of the output it spends.
				log.Debugf("Miner SSFee UTXO %v pays to a different script - creating new UTXO instead", outpoint)
			} else if !generator.isSSFeeOutputOfType(outpoint, blockHeight, blockIndex, "MF") {
				log.Debugf("Miner SSFee UTXO %v is not a miner SSFee output - creating new UTXO instead", outpoint)
			} else {
				// UTXO is available - safe to use for augmentation
				inputOutpoint = outpoint
				augmentValue = value
				existingBlockHeight = blockHeight
				existingBlockIndex = blockIndex
				log.Debugf("Found augmentable miner SSFee UTXO %v (value=%v, height=%d, index=%d) for coin type %d",
					outpoint, value, blockHeight, blockIndex, coinType)
			}
		} else if outpoint == nil {
			log.Debugf("No existing miner SSFee UTXO found in SSFeeIndex for hash160 %x (will create new UTXO)",
				minerHash160)
		}
	}

	// Create the miner SSFee transaction
	tx := wire.NewMsgTx()
	tx.Version = 3 // Same version as staker SSFee

	// Add input: either real UTXO (augmentation) or null input (creation)
	// SignatureScript must be explicitly empty (not nil) for validation
	if inputOutpoint != nil && augmentValue != nil && augmentValue.Sign() > 0 {
		// UTXO augmentation: use real input with fraud proof data
		txIn := &wire.TxIn{
			PreviousOutPoint: *inputOutpoint,
			Sequence:         wire.MaxTxInSequenceNum,
			BlockHeight:      uint32(existingBlockHeight),
			BlockIndex:       existingBlockIndex,
			SignatureScript:  []byte{}, // SSFee inputs carry no signature script
		}
		// SKA uses SKAValueIn (bigint), VAR uses ValueIn (int64)
		if coinType.IsSKA() {
			txIn.SKAValueIn = new(big.Int).Set(augmentValue)
		} else {
			txIn.ValueIn = augmentValue.Int64()
		}
		tx.AddTxIn(txIn)
	} else {
		// Fallback: create new UTXO with null input (like coinbase/treasurybase)
		txIn := &wire.TxIn{
			PreviousOutPoint: *wire.NewOutPoint(&chainhash.Hash{},
				wire.MaxPrevOutIndex, wire.TxTreeRegular),
			Sequence:        wire.MaxTxInSequenceNum,
			BlockHeight:     wire.NullBlockHeight,
			BlockIndex:      wire.NullBlockIndex,
			SignatureScript: []byte{},
		}
		// SKA uses SKAValueIn (bigint), VAR uses ValueIn (int64)
		if coinType.IsSKA() {
			txIn.SKAValueIn = new(big.Int).Set(totalFee)
		} else {
			txIn.ValueIn = totalFee.Int64()
		}
		tx.AddTxIn(txIn)
	}

	// Add OP_RETURN output for unique hash (output[0] - consistent with voter SSFee)
	// OP_RETURN output: Value=0 for VAR, SKAValue=0 for SKA
	opReturnOut := &wire.TxOut{
		CoinType: coinType,
		PkScript: opReturnScript,
	}
	if coinType.IsSKA() {
		opReturnOut.SKAValue = new(big.Int) // Zero value for SKA OP_RETURN
	}
	tx.AddTxOut(opReturnOut)

	// Create payment output to miner (output[1] - consistent with voter SSFee)
	// Calculate output value using big.Int arithmetic (works for both VAR and SKA)
	outputValue := new(big.Int).Set(totalFee)
	if augmentValue != nil && augmentValue.Sign() > 0 {
		outputValue.Add(outputValue, augmentValue)
	}

	minerTxOut := &wire.TxOut{
		CoinType: coinType,
		Version:  scriptVersion,
		PkScript: payScript,
	}
	if coinType.IsSKA() {
		minerTxOut.SKAValue = outputValue
	} else {
		// VAR values fit in int64
		minerTxOut.Value = outputValue.Int64()
	}
	tx.AddTxOut(minerTxOut)

	retTx := dcrutil.NewTx(tx)
	retTx.SetTree(wire.TxTreeStake)

	// Mark the UTXO as in-flight if we used one for augmentation
	if inputOutpoint != nil && generator != nil {
		generator.markSSFeeUTXOInFlight(*inputOutpoint, nextBlockHeight)
	}

	return retTx, nil
}

// spendTransaction updates the passed view by marking the inputs to the passed
// transaction as spent.  It also adds all outputs in the passed transaction
// which are not provably unspendable as available unspent transaction outputs.
func spendTransaction(utxoView *blockchain.UtxoViewpoint, tx *dcrutil.Tx,
	height int64, isTreasuryEnabled bool) {

	for _, txIn := range tx.MsgTx().TxIn {
		entry := utxoView.LookupEntry(txIn.PreviousOutPoint)
		if entry != nil {
			entry.Spend()
		}
	}

	utxoView.AddTxOuts(tx, height, wire.NullBlockIndex, isTreasuryEnabled)
}

// logSkippedDeps logs any dependencies which are also skipped as a result of
// skipping a transaction while generating a block template at the trace level.
func logSkippedDeps(tx *dcrutil.Tx, deps []*TxDesc) {
	if deps == nil {
		return
	}

	for _, item := range deps {
		log.Tracef("Skipping tx %s since it depends on %s\n",
			item.Tx.Hash(), tx.Hash())
	}
}

// minimumMedianTime returns the minimum allowed timestamp for a block building
// on the end of the current best chain.  In particular, it is one second after
// the median timestamp of the last several blocks per the chain consensus
// rules.
func minimumMedianTime(best *blockchain.BestState) time.Time {
	return best.MedianTime.Add(time.Second)
}

// medianAdjustedTime returns the current time adjusted to ensure it is at least
// one second after the median timestamp of the last several blocks per the
// chain consensus rules.
func (g *BlkTmplGenerator) medianAdjustedTime() time.Time {
	// The timestamp for the block must not be before the median timestamp
	// of the last several blocks.  Thus, choose the maximum between the
	// current time and one second after the past median time.  The current
	// timestamp is truncated to a second boundary before comparison since a
	// block timestamp does not support a precision greater than one second.
	best := g.cfg.BestSnapshot()
	newTimestamp := g.cfg.TimeSource.AdjustedTime()
	minTimestamp := minimumMedianTime(best)
	if newTimestamp.Before(minTimestamp) {
		newTimestamp = minTimestamp
	}

	// Adjust by the amount requested from the command line argument.
	newTimestamp = newTimestamp.Add(
		time.Duration(-g.cfg.MiningTimeOffset) * time.Second)

	return newTimestamp
}

// maybeInsertStakeTx checks to make sure that a stake tx is
// valid from the perspective of the mainchain (not necessarily
// the mempool or block) before inserting into a tx tree.
// If it fails the check, it returns false; otherwise true.
func (g *BlkTmplGenerator) maybeInsertStakeTx(stx *dcrutil.Tx, treeValid bool, isTreasuryEnabled bool) bool {
	missingInput := false

	view, err := g.cfg.FetchUtxoView(stx, treeValid)
	if err != nil {
		log.Warnf("Unable to fetch transaction store for "+
			"stx %s: %v", stx.Hash(), err)
		return false
	}
	mstx := stx.MsgTx()
	isSSGen := stake.IsSSGen(mstx)
	var isTSpend, isTreasuryBase bool
	if isTreasuryEnabled {
		isTSpend = stake.IsTSpend(mstx)
		isTreasuryBase = stake.IsTreasuryBase(mstx)
	}
	for i, txIn := range mstx.TxIn {
		// Evaluate if this is a stakebase or treasury base input or
		// not. If it is, continue without evaluation of the input.
		if (i == 0 && (isSSGen || isTreasuryBase)) || isTSpend {
			txIn.BlockHeight = wire.NullBlockHeight
			txIn.BlockIndex = wire.NullBlockIndex
			continue
		}

		entry := view.LookupEntry(txIn.PreviousOutPoint)
		if entry == nil {
			missingInput = true
			break
		} else {
			txIn.ValueIn = entry.Amount()
			txIn.SKAValueIn = entry.SKAAmount() // For SKA UTXOs
			txIn.BlockHeight = uint32(entry.BlockHeight())
			txIn.BlockIndex = entry.BlockIndex()
		}
	}
	return !missingInput
}

// handleTooFewVoters handles the situation in which there are too few voters on
// of the blockchain. If there are too few voters and a cached parent template to
// work off of is present, it will return a copy of that template to pass to the
// miner.
// Safe for concurrent access.
func (g *BlkTmplGenerator) handleTooFewVoters(nextHeight int64,
	miningAddress stdaddr.Address, isTreasuryEnabled bool,
	subsidySplitVariant standalone.SubsidySplitVariant) (*BlockTemplate, error) {

	stakeValidationHeight := g.cfg.ChainParams.StakeValidationHeight

	// Handle not enough voters being present if we're set to mine aggressively
	// (default behavior).
	best := g.cfg.BestSnapshot()
	if nextHeight >= stakeValidationHeight && g.cfg.Policy.AggressiveMining {
		// Fetch the latest block and head and begin working off of it with an
		// empty transaction tree regular and the contents of that stake tree.
		// In the future we should have the option of reading some transactions
		// from this block, too.
		topBlock, err := g.cfg.BlockByHash(&best.Hash)
		if err != nil {
			str := fmt.Sprintf("unable to get tip block %s", best.PrevHash)
			return nil, makeError(ErrGetTopBlock, str)
		}
		tipHeader := &topBlock.MsgBlock().Header

		// Start with a copy of the tip block header.
		var block wire.MsgBlock
		block.Header = *tipHeader

		// Create and populate a new coinbase.
		coinbaseScript := make([]byte, len(coinbaseFlags)+2)
		copy(coinbaseScript[2:], coinbaseFlags)
		opReturnPkScript, err := standardCoinbaseOpReturn(tipHeader.Height)
		if err != nil {
			return nil, err
		}
		coinbaseTx := createCoinbaseTx(g.cfg.SubsidyCache, coinbaseScript,
			opReturnPkScript, topBlock.Height(), miningAddress,
			tipHeader.Voters, g.cfg.ChainParams, isTreasuryEnabled,
			subsidySplitVariant)

		// Copy the miner's VAR fee share from the tip's coinbase.
		// The tip's coinbase already includes fees from the same regular
		// transactions that are being copied into this replacement block,
		// so the fee amount is identical. Without this, the miner's fee
		// share is permanently lost while the staker's share (SSFee) is
		// preserved from the copied stake transactions.
		powOutputIdx := 2
		if isTreasuryEnabled {
			powOutputIdx = 1
		}
		tipCoinbaseTx := topBlock.Transactions()[0].MsgTx()
		if len(tipCoinbaseTx.TxOut) > powOutputIdx &&
			len(coinbaseTx.MsgTx().TxOut) > powOutputIdx {

			minerFees := tipCoinbaseTx.TxOut[powOutputIdx].Value -
				coinbaseTx.MsgTx().TxOut[powOutputIdx].Value
			if minerFees > 0 {
				coinbaseTx.MsgTx().TxOut[powOutputIdx].Value += minerFees
				log.Debugf("handleTooFewVoters: added %d atoms miner "+
					"fee share to coinbase at height %d",
					minerFees, tipHeader.Height)
			}
		}

		block.AddTransaction(coinbaseTx.MsgTx())

		if isTreasuryEnabled {
			treasuryBase, err := createTreasuryBaseTx(g.cfg.SubsidyCache,
				topBlock.Height(), tipHeader.Voters)
			if err != nil {
				return nil, err
			}
			block.AddSTransaction(treasuryBase.MsgTx())
		}

		// Copy all of the regular transactions over.
		for i, tx := range topBlock.Transactions() {
			if i == 0 {
				// Skip copying coinbase.
				continue
			}
			block.AddTransaction(tx.MsgTx())
		}

		// Copy all of the stake transactions over.
		for i, stx := range topBlock.STransactions() {
			if i == 0 && isTreasuryEnabled {
				// Skip copying treasurybase.
				continue
			}
			block.AddSTransaction(stx.MsgTx())
		}

		// Set a fresh timestamp.
		ts := g.medianAdjustedTime()
		block.Header.Timestamp = ts

		// Recalculate the size.
		block.Header.Size = uint32(block.SerializeSize())

		bt := &BlockTemplate{
			Block:           &block,
			Fees:            []*big.Int{big.NewInt(0)},
			SigOpCounts:     []int64{0},
			Height:          int64(tipHeader.Height),
			ValidPayAddress: miningAddress != nil,
		}

		// Calculate the merkle root depending on the result of the header
		// commitments agenda vote.
		prevHash := &tipHeader.PrevBlock
		hdrCmtActive, err := g.cfg.IsHeaderCommitmentsAgendaActive(prevHash)
		if err != nil {
			return nil, err
		}
		header := &block.Header
		calculatedMerkleRoot := calcBlockMerkleRoot(block.Transactions,
			block.STransactions, hdrCmtActive)
		header.MerkleRoot = calculatedMerkleRoot

		// Calculate the required difficulty for the block.
		reqDifficulty, err := g.cfg.CalcNextRequiredDifficulty(prevHash, ts)
		if err != nil {
			return nil, makeError(ErrGettingDifficulty, err.Error())
		}
		header.Bits = reqDifficulty

		// Calculate the stake root or commitment root depending on the result
		// of the header commitments agenda vote.
		var cmtRoot chainhash.Hash
		if hdrCmtActive {
			// Load all of the previous output scripts the block references as
			// inputs since they are needed to create the filter commitment.
			blockUtxos, err := g.cfg.FetchUtxoViewParentTemplate(&block)
			if err != nil {
				str := fmt.Sprintf("failed to fetch inputs when making new "+
					"block template: %v", err)
				return nil, makeError(ErrFetchTxStore, str)
			}

			cmtRoot, err = calcBlockCommitmentRootV1(&block, blockUtxos)
			if err != nil {
				str := fmt.Sprintf("failed to calculate commitment root for "+
					"block when making new block template: %v", err)
				return nil, makeError(ErrCalcCommitmentRoot, str)
			}
		} else {
			cmtRoot = standalone.CalcTxTreeMerkleRoot(block.STransactions)
		}
		header.StakeRoot = cmtRoot

		// Make sure the block validates.
		btBlock := dcrutil.NewBlockDeepCopyCoinbase(&block)
		err = g.cfg.CheckConnectBlockTemplate(btBlock)
		if err != nil {
			str := fmt.Sprintf("failed to check template: %v while "+
				"constructing a new parent", err.Error())
			return nil, makeError(ErrCheckConnectBlock, str)
		}

		return bt, nil
	}

	log.Debugf("Not enough voters on top block to generate " +
		"new block template")

	return nil, nil
}

// createRevocationFromTicket creates a revocation transaction from the provided
// ticket hash.  It also adds any utxos used to create the transaction to the
// provided block utxos viewpoint so that they are available for lookup.
//
// Note: This should only be used if the automatic ticket revocations agenda is
// active.
func (g *BlkTmplGenerator) createRevocationFromTicket(ticketHash *chainhash.Hash,
	blockUtxos *blockchain.UtxoViewpoint, prevHeaderBytes []byte,
	isTreasuryEnabled bool) (*TxDesc, error) {

	// Fetch the utxo for the ticket submission to be revoked.
	const ticketSubmissionOutput = 0
	ticketSubmission := wire.OutPoint{
		Hash:  *ticketHash,
		Index: ticketSubmissionOutput,
		Tree:  wire.TxTreeStake,
	}
	ticketUtxo, err := g.cfg.FetchUtxoEntry(ticketSubmission)
	if err != nil {
		return nil, makeError(ErrGetTicketInfo, err.Error())
	}
	if ticketUtxo == nil || ticketUtxo.IsSpent() {
		str := fmt.Sprintf("ticket %v does not exist or is spent", ticketHash)
		return nil, makeError(ErrGetTicketInfo, str)
	}

	// Add the ticket submission utxo to the block utxos view so that it is
	// available for lookup later.
	blockUtxos.Entries()[ticketSubmission] = ticketUtxo

	// Get the minimal outputs for the ticket.
	ticketMinOuts := ticketUtxo.TicketMinimalOutputs()
	if ticketMinOuts == nil {
		str := fmt.Sprintf("ticket %v missing minimal outputs", ticketHash)
		return nil, makeError(ErrGetTicketInfo, str)
	}

	// Create a revocation transaction for the ticket.
	const isAutoRevocationsEnabled = true
	const revocationTxFee = dcrutil.Amount(0)
	revocationMsgTx, err := stake.CreateRevocationFromTicket(ticketHash,
		ticketMinOuts, revocationTxFee, stake.TxVersionAutoRevocations,
		g.cfg.ChainParams, prevHeaderBytes, isAutoRevocationsEnabled)
	if err != nil {
		return nil, err
	}
	revocationTx := dcrutil.NewTx(revocationMsgTx)
	revocationTx.SetTree(wire.TxTreeStake)
	txDesc := &TxDesc{
		Tx:   revocationTx,
		Type: stake.TxTypeSSRtx,
		TotalSigOps: g.cfg.CountSigOps(revocationTx, false, false,
			isTreasuryEnabled),
		TxSize: int64(revocationMsgTx.SerializeSize()),
	}

	return txDesc, nil
}

// addAutoRevocationsToQueue is a helper function that creates revocations for
// all tickets that will become missed or expired as of the block being created.
// It inserts the created revocations into the provided priority queue and maps.
//
// Note: This should only be used if the automatic ticket revocations agenda is
// active.
func (g *BlkTmplGenerator) addAutoRevocationsToQueue(winningTickets map[chainhash.Hash]bool,
	blockUtxos *blockchain.UtxoViewpoint, prevHeaderBytes []byte, numSSGen int,
	isTreasuryEnabled bool, priorityQueue *txPriorityQueue,
	prioritizedTxns map[chainhash.Hash]struct{},
	prioItemMap map[chainhash.Hash]*txPrioItem) error {

	// Return now if there are no tickets to revoke.
	best := g.cfg.BestSnapshot()
	missedCount := len(best.NextWinningTickets) - numSSGen
	expiredCount := len(best.NextExpiringTickets)
	if missedCount+expiredCount == 0 {
		return nil
	}

	// Create a slice of the ticket hashes that must be revoked due to
	// becoming missed or expired this block.
	revokeTickets := make([]chainhash.Hash, 0, missedCount+expiredCount)

	// Add tickets that will be missed to the slice of ticket hashes to
	// revoke.
	for ticketHash, hasVote := range winningTickets {
		// If a winning ticket does not have a vote in this block, it will be
		// missed.
		if !hasVote {
			revokeTickets = append(revokeTickets, ticketHash)
		}
	}

	// Add tickets that will be expired to the slice of ticket hashes to
	// revoke.
	revokeTickets = append(revokeTickets, best.NextExpiringTickets...)

	// Create revocation transactions for all tickets that will become missed
	// or expired as of this block.
	for i := range revokeTickets {
		// Create the revocation transaction.
		ticketHash := &revokeTickets[i]
		txDesc, err := g.createRevocationFromTicket(ticketHash, blockUtxos,
			prevHeaderBytes, isTreasuryEnabled)
		if err != nil {
			return err
		}

		// Add the revocation transaction to the priority queue.
		prioItem := &txPrioItem{
			txDesc:         txDesc,
			txType:         txDesc.Type,
			autoRevocation: true,
		}
		revocationTxHash := txDesc.Tx.Hash()
		prioritizedTxns[*revocationTxHash] = struct{}{}
		heap.Push(priorityQueue, prioItem)
		prioItemMap[*revocationTxHash] = prioItem
	}

	log.Debugf("Generated %d automatic revocations for the new block (num "+
		"missed this block: %d, num expired this block: %d)", len(revokeTickets),
		missedCount, expiredCount)

	return nil
}

// BlkTmplGenerator generates block templates based on a given mining policy
// and a transactions source. It also houses additional state required in
// order to ensure the templates adhere to the consensus rules and are built
// on top of the best chain tip or its parent if the best chain tip is
// unable to get enough votes.
//
// See the NewBlockTemplate method for a detailed description of how the block
// template is generated.
type BlkTmplGenerator struct {
	cfg *Config

	// inFlightSSFeeUTXOs tracks UTXOs currently used in pending block templates.
	// These are cleared when a new block is connected (template mined or abandoned).
	// Key: OutPoint being spent, Value: block height the template was created for
	inFlightSSFeeUTXOs map[wire.OutPoint]int64
	inFlightMtx        sync.Mutex
}

// NewBlkTmplGenerator returns a new block template generator for the given
// policy using transactions from the provided transaction source.
func NewBlkTmplGenerator(cfg *Config) *BlkTmplGenerator {
	return &BlkTmplGenerator{
		cfg:                cfg,
		inFlightSSFeeUTXOs: make(map[wire.OutPoint]int64),
	}
}

// ClearInFlightSSFeeUTXOs clears all in-flight SSFee UTXOs for heights <= the
// given block height. Called when a block is connected to clear obsolete entries.
func (g *BlkTmplGenerator) ClearInFlightSSFeeUTXOs(blockHeight int64) {
	g.inFlightMtx.Lock()
	for outpoint, height := range g.inFlightSSFeeUTXOs {
		if height <= blockHeight {
			delete(g.inFlightSSFeeUTXOs, outpoint)
		}
	}
	g.inFlightMtx.Unlock()
}

// isSSFeeOutputOfType returns whether the output was created by an SSFee
// transaction of the given type (SF or MF), located in the main chain at the
// given height and stake tree index.  Consensus only lets an SSFee augment an
// output of an SSFee of its own type, and the SSFee index can file an output
// under the other type, so the type is read from the transaction itself.  It
// returns false when the type cannot be confirmed.
func (g *BlkTmplGenerator) isSSFeeOutputOfType(outpoint *wire.OutPoint,
	height int64, index uint32, feeType string) bool {

	if g == nil || g.cfg.BlockByHeight == nil {
		return false
	}
	block, err := g.cfg.BlockByHeight(height)
	if err != nil {
		log.Debugf("Failed to fetch block at height %d for SSFee UTXO %v: %v",
			height, outpoint, err)
		return false
	}
	stxns := block.STransactions()
	if int(index) >= len(stxns) || *stxns[index].Hash() != outpoint.Hash {
		return false
	}
	srcFeeType, err := blockchain.SSFeeType(stxns[index].MsgTx())
	return err == nil && srcFeeType == feeType
}

// isSSFeeUTXOInFlight returns true if the UTXO is already being used in a pending
// block template for the current or recent height.
func (g *BlkTmplGenerator) isSSFeeUTXOInFlight(outpoint wire.OutPoint, currentHeight int64) bool {
	g.inFlightMtx.Lock()
	defer g.inFlightMtx.Unlock()
	height, exists := g.inFlightSSFeeUTXOs[outpoint]
	return exists && height >= currentHeight-1
}

// markSSFeeUTXOInFlight marks a UTXO as being used in a pending block template.
func (g *BlkTmplGenerator) markSSFeeUTXOInFlight(outpoint wire.OutPoint, blockHeight int64) {
	g.inFlightMtx.Lock()
	g.inFlightSSFeeUTXOs[outpoint] = blockHeight
	g.inFlightMtx.Unlock()
}

// calcFeePerKb returns an adjusted fee per kilobyte taking the provided
// transaction and its ancestors into account. Returns *big.Int (atoms per KB)
// to support both VAR and SKA transactions with full precision.
func calcFeePerKb(txDesc *TxDesc, ancestorStats *TxAncestorStats) *big.Int {
	txSize := int64(txDesc.Tx.MsgTx().SerializeSize())

	// Get fee as big.Int (use SKAFee if available, otherwise Fee)
	var fee *big.Int
	if txDesc.SKAFee != nil {
		fee = new(big.Int).Set(txDesc.SKAFee)
	} else {
		fee = big.NewInt(txDesc.Fee)
	}

	// Add ancestor fees if valid
	if ancestorStats.Fees >= 0 && ancestorStats.SizeBytes >= 0 {
		fee = fee.Add(fee, big.NewInt(ancestorStats.Fees))
		txSize += ancestorStats.SizeBytes
	}

	// Calculate fee per KB: (fee * 1000) / size
	result := new(big.Int).Mul(fee, big.NewInt(kilobyte))
	result.Div(result, big.NewInt(txSize))
	return result
}

// calcCoinTypeAwareFeePerKb returns a coin-type-adjusted fee per kilobyte that
// takes into account the economic dynamics and network conditions specific to
// the transaction's coin type. Returns *big.Int (atoms per KB) for full precision.
func calcCoinTypeAwareFeePerKb(txDesc *TxDesc, ancestorStats *TxAncestorStats,
	coinType cointype.CoinType, feeCalc *fees.CoinTypeFeeCalculator) *big.Int {

	// Get base fee rate using standard calculation
	baseFeeRate := calcFeePerKb(txDesc, ancestorStats)

	// Get the estimated fee rate for this coin type with fast confirmation (1 block)
	estimatedRate, err := feeCalc.EstimateFeeRate(coinType, 1)
	if err != nil {
		// Fallback to base calculation if estimation fails
		return baseFeeRate
	}

	// Use the higher of actual fee rate or coin-type minimum for prioritization
	// This ensures that transactions paying coin-type-appropriate fees get priority
	if baseFeeRate.Cmp(estimatedRate) >= 0 {
		// Transaction is paying above coin-type minimum, use actual rate
		return baseFeeRate
	}

	// Transaction is paying below coin-type expectations.
	// The adjustment (baseFeeRate * estimatedRate / estimatedRate) equals baseFeeRate,
	// so just return the base rate - proportional priority within its tier.
	return baseFeeRate
}

// NewBlockTemplate returns a new block template that is ready to be solved
// using the transactions from the passed transaction source pool and a coinbase
// that either pays to the passed address if it is not nil, or a coinbase that
// is redeemable by anyone if the passed address is nil.  The nil address
// functionality is useful since there are cases such as the getblocktemplate
// RPC where external mining software is responsible for creating their own
// coinbase which will replace the one generated for the block template.  Thus
// the need to have configured address can be avoided.
//
// The transactions selected and included are prioritized according to whether
// or not they are stake transactions and their fee per kilobyte.  Transactions
// with a higher fee per kilobyte are preferred.  Finally, the block generation
// related policy settings are all taken into account.
//
// Transactions which only spend outputs from other transactions already in the
// block chain are immediately added to a priority queue which prioritizes based
// the fee per kilobyte.  Transactions which spend outputs from other
// transactions in the source pool are added to a dependency map so they can be
// added to the priority queue once the transactions they depend on have been
// included.
//
// When the fees per kilobyte drop below the TxMinFreeFee policy setting, the
// transaction will be skipped.
//
// Any transactions which would cause the block to exceed the BlockMaxSize
// policy setting, exceed the maximum allowed signature operations per block, or
// otherwise cause the block to be invalid are skipped.
//
// Given the above, a block generated by this function is of the following form:
//
//	 -----------------------------------  --
//	|      Coinbase Transaction         |   |
//	|-----------------------------------|   |
//	|                                   |   |
//	|                                   |   |--- (policy.BlockMaxSize) / 2
//	|  Transactions prioritized by fee  |   |
//	|  until <= policy.TxMinFreeFee     |   |
//	|                                   |   |
//	|                                   |   |
//	 -----------------------------------  --
//
// Which also includes a stake tree that looks like the following:
//
//	 -----------------------------------  --  --
//	|                                   |   |   |
//	|             Votes                 |   |   | --- >= (chaincfg.TicketsPerBlock/2) + 1
//	|                                   |   |   |
//	|-----------------------------------|   | --
//	|                                   |   |   |
//	|            Tickets                |   |   | --- <= chaincfg.MaxFreshStakePerBlock
//	|                                   |   |   |
//	|-----------------------------------|   | --
//	|                                   |   |
//	|          Revocations              |   |
//	|                                   |   |
//	 -----------------------------------  --
//
// This function returns nil when there are not enough voters on any of the
// current top blocks to create a new block template.
func (g *BlkTmplGenerator) NewBlockTemplate(payToAddress stdaddr.Address) (*BlockTemplate, error) {
	// All transaction scripts are verified using the more strict standard
	// flags.
	scriptFlags, err := g.cfg.Policy.StandardVerifyFlags()
	if err != nil {
		return nil, err
	}

	// Extend the most recently known best block.
	// The most recently known best block is the top block that has the most
	// ssgen votes for it. We only need this after the height in which stake voting
	// has kicked in.
	// To figure out which block has the most ssgen votes, we need to run the
	// following algorithm:
	// 1. Acquire the HEAD block and all of its orphans. Record their block header
	// hashes.
	// 2. Create a map of [blockHeaderHash] --> [mempoolTxnList].
	// 3. for blockHeaderHash in candidateBlocks:
	//		if mempoolTx.StakeDesc == SSGen &&
	//			mempoolTx.SSGenParseBlockHeader() == blockHeaderHash:
	//			map[blockHeaderHash].append(mempoolTx)
	// 4. Check len of each map entry and store.
	// 5. Query the ticketdb and check how many eligible ticket holders there are
	//    for the given block you are voting on.
	// 6. Divide #ofvotes (len(map entry)) / totalPossibleVotes --> penalty ratio
	// 7. Store penalty ratios for all block candidates.
	// 8. Select the one with the largest penalty ratio (highest block reward).
	//    This block is then selected to build upon instead of the others, because
	//    it yields the greater amount of rewards.

	best := g.cfg.BestSnapshot()
	prevHash := best.Hash
	nextBlockHeight := best.Height + 1
	stakeValidationHeight := g.cfg.ChainParams.StakeValidationHeight

	isTreasuryEnabled, err := g.cfg.IsTreasuryAgendaActive(&prevHash)
	if err != nil {
		return nil, err
	}

	isAutoRevocationsEnabled, err := g.cfg.IsAutoRevocationsAgendaActive(&prevHash)
	if err != nil {
		return nil, err
	}

	// Determine which subsidy split variant to use depending on the active
	// agendas.
	subsidySplitVariant := standalone.SSVMonetarium

	var (
		isTVI            bool
		maxTreasurySpend int64
	)
	if isTreasuryEnabled {
		isTVI = standalone.IsTreasuryVoteInterval(uint64(nextBlockHeight),
			g.cfg.ChainParams.TreasuryVoteInterval)
	}

	if nextBlockHeight >= stakeValidationHeight {
		// Obtain the entire generation of blocks stemming from this parent.
		children := g.cfg.TipGeneration()

		// Get the list of blocks that we can actually build on top of. If we're
		// not currently on the block that has the most votes, switch to that
		// block.
		eligibleParents := SortParentsByVotes(g.cfg.TxSource, prevHash, children,
			g.cfg.ChainParams)
		if len(eligibleParents) == 0 {
			log.Debugf("Too few voters found on any HEAD block, " +
				"recycling a parent block to mine on")
			return g.handleTooFewVoters(nextBlockHeight, payToAddress,
				isTreasuryEnabled, subsidySplitVariant)
		}

		log.Debugf("Found eligible parent %v with enough votes to build "+
			"block on, proceeding to create a new block template",
			eligibleParents[0])

		// Force a reorganization to the parent with the most votes if needed.
		for i := range eligibleParents {
			newHead := &eligibleParents[i]
			if *newHead == prevHash {
				break
			}

			err := g.cfg.ForceHeadReorganization(prevHash, *newHead)
			if err != nil {
				log.Debugf("failed to reorganize to new parent: %v", err)
				continue
			}

			// Ensure the needed votes are actually in the mempool.
			voteHashes := g.cfg.TxSource.VoteHashesForBlock(newHead)
			if len(voteHashes) == 0 {
				return nil, fmt.Errorf("no vote metadata for block %v",
					newHead)
			}
			haveAllVotes := g.cfg.TxSource.HaveAllTransactions(voteHashes)
			if !haveAllVotes {
				continue
			}

			prevHash = *newHead
			break
		}

		// Obtain the maximum allowed treasury expenditure.
		if isTreasuryEnabled && isTVI {
			maxTreasurySpend, err = g.cfg.MaxTreasuryExpenditure(&prevHash)
			if err != nil {
				return nil, err
			}
		}
	}

	// Get the current source transactions and create a priority queue to
	// hold the transactions which are ready for inclusion into a block
	// along with some priority related and fee metadata.  Reserve the same
	// number of items that are available for the priority queue.  Also,
	// choose the initial sort order for the priority queue based on whether
	// or not there is an area allocated for high-priority transactions.
	miningView := g.cfg.TxSource.MiningView()
	sourceTxns := miningView.TxDescs()
	// Use coin-type-aware prioritization when fee calculator is available
	var prioritizationFunc txPriorityQueueLessFunc
	if g.cfg.FeeCalculator != nil {
		prioritizationFunc = txPQByCoinTypeAndFee
	} else {
		prioritizationFunc = txPQByStakeAndFee
	}
	priorityQueue := newTxPriorityQueue(len(sourceTxns), prioritizationFunc)
	prioritizedTxns := make(map[chainhash.Hash]struct{}, len(sourceTxns))

	// Create a slice to hold the transactions to be included in the
	// generated block with reserved space.  Also create a utxo view to
	// house all of the input transactions so multiple lookups can be
	// avoided.
	blockTxns := make([]*dcrutil.Tx, 0, len(sourceTxns))
	blockUtxos := g.cfg.NewUtxoViewpoint()

	// Create slices to hold the fees and number of signature operations
	// for each of the selected transactions and add an entry for the
	// coinbase.  This allows the code below to simply append details about
	// a transaction as it is selected for inclusion in the final block.
	// However, since the total fees aren't known yet, use a dummy value for
	// the coinbase fee which will be updated later.
	txFees := make([]*big.Int, 0, len(sourceTxns))
	txFeesMap := make(map[chainhash.Hash]*big.Int) // Use big.Int to support SKA fees
	txSigOpCounts := make([]int64, 0, len(sourceTxns))
	txSigOpCountsMap := make(map[chainhash.Hash]int64)
	txFees = append(txFees, big.NewInt(-1)) // Updated once known

	log.Debugf("Considering %d transactions for inclusion to new block",
		len(sourceTxns))
	knownDisapproved := g.cfg.TxSource.IsRegTxTreeKnownDisapproved(&prevHash)

	// Tracks the total number of transactions that depend on another
	// from the tx source.
	totalDescendantTxns := 0
	prioItemMap := make(map[chainhash.Hash]*txPrioItem, len(sourceTxns))

mempoolLoop:
	for _, txDesc := range sourceTxns {
		// A block can't have more than one coinbase or contain
		// non-finalized transactions.
		tx := txDesc.Tx
		msgTx := tx.MsgTx()
		if standalone.IsCoinBaseTx(msgTx, isTreasuryEnabled) {
			log.Tracef("Skipping coinbase tx %s", tx.Hash())
			continue
		}
		if !g.cfg.IsFinalizedTransaction(tx, nextBlockHeight, best.MedianTime) {
			log.Tracef("Skipping non-finalized tx %s", tx.Hash())
			continue
		}

		// Need this for a check below for stake base input, and to check
		// the ticket number.
		isSSGen := txDesc.Type == stake.TxTypeSSGen
		if isSSGen {
			blockHash, blockHeight := stake.SSGenBlockVotedOn(msgTx)
			if !((blockHash == prevHash) &&
				(int64(blockHeight) == nextBlockHeight-1)) {
				log.Tracef("Skipping ssgen tx %s because it does "+
					"not vote on the correct block", tx.Hash())
				continue
			}
		}

		var isTSpend bool
		if isTreasuryEnabled {
			isTSpend = txDesc.Type == stake.TxTypeTSpend
		}

		// Fetch all of the utxos referenced by the this transaction.
		utxos, err := g.cfg.FetchUtxoView(tx, !knownDisapproved)
		if err != nil {
			log.Warnf("Unable to fetch utxo view for tx %s: "+
				"%v", tx.Hash(), err)
			continue
		}

		// Setup dependencies for any transactions which reference
		// other transactions in the mempool so they can be properly
		// ordered below.

		// Determine the primary coin type for this transaction
		primaryCoinType := blockalloc.GetTransactionCoinType(tx)

		// Check if this is an SKA emission transaction
		isSKAEmission := wire.IsSKAEmissionTransaction(msgTx)

		prioItem := &txPrioItem{
			txDesc:        txDesc,
			txType:        txDesc.Type,
			coinType:      primaryCoinType,
			isSKAEmission: isSKAEmission,
		}
		for i, txIn := range tx.MsgTx().TxIn {
			// Evaluate if this is a stakebase input or not. If it is, continue
			// without evaluation of the input.
			// if isStakeBase
			isSKAEmissionForUTXO := wire.IsSKAEmissionTransaction(tx.MsgTx())
			if (i == 0 && isSSGen) || isTSpend || isSKAEmissionForUTXO {
				continue
			}

			originHash := &txIn.PreviousOutPoint.Hash
			entry := utxos.LookupEntry(txIn.PreviousOutPoint)
			if entry == nil || entry.IsSpent() {
				if !g.cfg.TxSource.HaveTransaction(originHash) {
					log.Tracef("Skipping tx %s because "+
						"it references unspent output "+
						"%s which is not available",
						tx.Hash(), txIn.PreviousOutPoint)
					continue mempoolLoop
				}
			}
		}

		// Calculate the final transaction priority using the input
		// value age sum as well as the adjusted transaction size.  The
		// formula is: sum(inputValue * inputAge) / adjustedTxSize
		prioItem.priority = CalcPriority(tx.MsgTx(), utxos,
			nextBlockHeight)

		// Calculate the fee in Atoms/KB using coin-type-specific calculation.
		// NOTE: This is a more precise value than the one calculated
		// during calcMinRelayFee which rounds up to the nearest full
		// kilobyte boundary.  This is beneficial since it provides an
		// incentive to create smaller transactions.
		ancestorStats, hasStats := miningView.AncestorStats(tx.Hash())

		// Use coin-type-aware fee calculation when available
		if g.cfg.FeeCalculator != nil {
			prioItem.feePerKB = calcCoinTypeAwareFeePerKb(txDesc, ancestorStats, prioItem.coinType, g.cfg.FeeCalculator)
		} else {
			// Fallback to standard calculation
			prioItem.feePerKB = calcFeePerKb(txDesc, ancestorStats)
		}
		prioItem.fee = txDesc.Fee + ancestorStats.Fees
		prioItemMap[*tx.Hash()] = prioItem
		hasParents := miningView.hasParents(tx.Hash())

		if !hasParents || hasStats {
			heap.Push(priorityQueue, prioItem)
			prioritizedTxns[*tx.Hash()] = struct{}{}
			blockUtxos.AddTxOuts(tx, nextBlockHeight, wire.NullBlockIndex,
				isTreasuryEnabled)
		}

		if hasParents {
			totalDescendantTxns++
		}

		// Merge the referenced outputs from the input transactions to
		// this transaction into the block utxo view.  This allows the
		// code below to avoid a second lookup.
		mergeUtxoView(blockUtxos, utxos)
	}

	log.Tracef("Priority queue len %d, dependers len %d",
		priorityQueue.Len(), totalDescendantTxns)

	// The starting block size is the size of the block header plus the max
	// possible transaction count size, plus the size of the coinbase
	// transaction.
	blockSize := uint32(blockHeaderOverhead)

	// Guesstimate for sigops based on valid txs in loop below. This number
	// tends to overestimate sigops because of the way the loop below is
	// coded and the fact that tx can sometimes be removed from the tx
	// trees if they fail one of the stake checks below the priorityQueue
	// pop loop. This is buggy, but not catastrophic behaviour. A future
	// release should fix it. TODO
	blockSigOps := int64(0)
	totalFees := wire.NewFeesByType()

	numSStx := 0
	numSSGen := 0
	numTAdds := 0

	foundWinningTickets := make(map[chainhash.Hash]bool, len(best.NextWinningTickets))
	for _, ticketHash := range best.NextWinningTickets {
		foundWinningTickets[ticketHash] = false
	}

	// Create a map of the ticket hashes that will become expired as of the block.
	numExpiring := len(best.NextExpiringTickets)
	expiringTicketHashes := make(map[chainhash.Hash]struct{}, numExpiring)
	for _, ticketHash := range best.NextExpiringTickets {
		expiringTicketHashes[ticketHash] = struct{}{}
	}

	// Maintain lookup of transactions that have been included in the block
	// template.
	templateTxnMap := make(map[chainhash.Hash]struct{})

	// Track if auto revocations have been added to the priority queue.
	addedAutoRevocations := false

	// Get the best block and header.
	bestHeader, err := g.cfg.HeaderByHash(&best.Hash)
	if err != nil {
		str := fmt.Sprintf("unable to get tip block header %v", best.Hash)
		return nil, makeError(ErrGetTopBlock, str)
	}
	bestHeaderBytes, err := bestHeader.Bytes()
	if err != nil {
		str := fmt.Sprintf("failed to serialize header for block %v: %v",
			best.Hash, err)
		return nil, makeError(ErrSerializeHeader, str)
	}

	// Initialize block space allocator for coin type-based space management
	var blockSpaceAllocator *BlockSpaceAllocator
	if g.cfg.BlockSpaceAllocator != nil {
		// Use configured allocator with fee calculator integration
		blockSpaceAllocator = g.cfg.BlockSpaceAllocator
	} else {
		// Create basic allocator for backward compatibility
		blockSpaceAllocator = NewBlockSpaceAllocator(g.cfg.Policy.BlockMaxSize, g.cfg.ChainParams)
	}

	// Calculate total pending transaction bytes from mempool for each coin type.
	// This provides visibility into the allocation decisions and helps with debugging.
	mempoolPendingBytes := make(map[cointype.CoinType]uint32)
	for _, txDesc := range sourceTxns {
		coinType := blockalloc.GetTransactionCoinType(txDesc.Tx)
		txSize := uint32(txDesc.Tx.MsgTx().SerializeSize())
		mempoolPendingBytes[coinType] += txSize
	}

	// Log initial allocation based on actual mempool demand.
	// This helps diagnose issues where SKA reserves space but has no transactions.
	if len(mempoolPendingBytes) > 0 {
		initialAlloc := blockSpaceAllocator.AllocateBlockSpace(mempoolPendingBytes)
		varAlloc := initialAlloc.GetAllocationForCoinType(cointype.CoinTypeVAR)
		varPending := mempoolPendingBytes[cointype.CoinTypeVAR]

		var varAllocSize uint32
		if varAlloc != nil {
			varAllocSize = varAlloc.FinalAllocation
		}

		log.Debugf("Block template mempool analysis: VAR pending=%d bytes, "+
			"VAR allocation=%d bytes, block max=%d bytes",
			varPending, varAllocSize, g.cfg.Policy.BlockMaxSize)

		// Log VAR allocation efficiency
		if varAllocSize > 0 && varPending > 0 {
			efficiency := float64(varPending) / float64(varAllocSize) * 100
			log.Debugf("VAR allocation efficiency: %.1f%% (%d used of %d allocated)",
				efficiency, varPending, varAllocSize)
		} else if varAllocSize > 0 && varPending == 0 {
			log.Debugf("VAR allocation: %d bytes allocated, but 0 bytes pending (allocation unused)",
				varAllocSize)
		}

		// Log SKA pending if any
		for coinType, pending := range mempoolPendingBytes {
			if coinType.IsSKA() && pending > 0 {
				skaAlloc := initialAlloc.GetAllocationForCoinType(coinType)
				var skaAllocSize uint32
				if skaAlloc != nil {
					skaAllocSize = skaAlloc.FinalAllocation
				}
				log.Debugf("Block template mempool analysis: %s pending=%d bytes, "+
					"%s allocation=%d bytes", coinType, pending, coinType, skaAllocSize)

				// Log SKA allocation efficiency
				if skaAllocSize > 0 {
					skaEfficiency := float64(pending) / float64(skaAllocSize) * 100
					log.Debugf("%s allocation efficiency: %.1f%% (%d used of %d allocated)",
						coinType, skaEfficiency, pending, skaAllocSize)
				}
			}
		}
	}

	transactionTracker := blockalloc.NewTransactionSizeTracker(blockSpaceAllocator.BlockSpaceAllocator)

	// Choose which transactions make it into the block.
nextPriorityQueueItem:
	for priorityQueue.Len() > 0 {
		log.Debugf("Priority queue length: %d", priorityQueue.Len())
		// Grab the highest priority (or highest fee per kilobyte
		// depending on the sort order) transaction.
		prioItem := heap.Pop(priorityQueue).(*txPrioItem)
		tx := prioItem.txDesc.Tx
		delete(prioritizedTxns, *tx.Hash())

		if _, exist := templateTxnMap[*tx.Hash()]; exist {
			continue
		}

		// Store if this is an SStx or not.
		isSStx := prioItem.txType == stake.TxTypeSStx

		// Store if this is an SSGen or not.
		isSSGen := prioItem.txType == stake.TxTypeSSGen

		// Store if this is an SSRtx or not.
		isSSRtx := prioItem.txType == stake.TxTypeSSRtx

		var isTSpend, isTAdd bool
		if isTreasuryEnabled {
			// Store if this is an TSpend or not.
			isTSpend = prioItem.txType == stake.TxTypeTSpend

			// Store if this is a TAdd or not
			isTAdd = prioItem.txType == stake.TxTypeTAdd
		}

		// If the transaction is not a vote, then we are done adding votes since
		// votes are the highest priority in the queue.
		doneAddingVotes := !isSSGen

		// If the automatic ticket revocations agenda is active, then the block MUST
		// contain version 2 revocation transactions for all tickets that will
		// become missed or expired as of this block.
		//
		// Create and insert these revocations into the priority queue AFTER votes
		// are done being added to ensure that revocations are added for votes that
		// fail validation checks or are otherwise not included into the block.
		if isAutoRevocationsEnabled && !addedAutoRevocations && doneAddingVotes {
			// Create revocations for all tickets that will become missed or expired
			// as of the block being created and add them to the priority queue and
			// maps.
			err = g.addAutoRevocationsToQueue(foundWinningTickets, blockUtxos,
				bestHeaderBytes, numSSGen, isTreasuryEnabled, priorityQueue,
				prioritizedTxns, prioItemMap)
			if err != nil {
				return nil, err
			}

			// Set the flag that auto revocations have been added so that they are
			// only added once.
			addedAutoRevocations = true

			// Requeue the current item now that the revocations have been added and
			// continue.
			heap.Push(priorityQueue, prioItem)
			prioritizedTxns[*tx.Hash()] = struct{}{}
			continue
		}

		// Grab the list of transactions which depend on this one (if any).
		deps := miningView.children(tx.Hash())

		// Skip TSpend if this block is not on a TVI or outside of the
		// Tspend window.
		if isTSpend {
			if !isTVI {
				log.Debugf("Skipping tspend %v because block "+
					"is not on a TVI: %v", tx.Hash(),
					nextBlockHeight)
				continue
			}

			// We are on a TVI, make sure this tspend is within the
			// window.
			exp := tx.MsgTx().Expiry
			if !standalone.InsideTSpendWindow(nextBlockHeight,
				exp, g.cfg.ChainParams.TreasuryVoteInterval,
				g.cfg.ChainParams.TreasuryVoteIntervalMultiplier) {

				log.Debugf("Skipping treasury spend %v at height %d because it "+
					"has an expiry of %d that is outside of the voting window",
					tx.Hash(), nextBlockHeight, exp)
				continue
			}

			// Ensure there are enough votes. Send in a fake block
			// with the proper height.
			err = g.cfg.CheckTSpendHasVotes(prevHash,
				dcrutil.NewTx(tx.MsgTx()))
			if err != nil {
				log.Debugf("Skipping tspend %v because it doesn't have enough "+
					"votes: height %v reason '%v'", tx.Hash(), nextBlockHeight, err)
				continue
			}

			// Ensure this TSpend won't overspend from the
			// treasury. This is currently considering between
			// TSpends by tx priority order, which might not be
			// ideal, but we don't expect this to be triggered for
			// mainnet. This could be improved by sorting approved
			// TSpends either by approval % or by expiry.
			tspendAmount := tx.MsgTx().TxIn[0].ValueIn
			if maxTreasurySpend-tspendAmount < 0 {
				log.Debugf("Skipping tspend %v because it spends "+
					"more than allowed: treasury %d tspend %d",
					tx.Hash(), maxTreasurySpend, tspendAmount)
				continue
			}
			maxTreasurySpend -= tspendAmount
		}

		// Skip if we already have too many TAdds.
		if isTAdd && numTAdds >= blockchain.MaxTAddsPerBlock {
			log.Debugf("Skipping tadd %s because it would exceed "+
				"the max number of tadds allowed in a block",
				tx.Hash())
			logSkippedDeps(tx, deps)
			continue
		}

		// Skip if we already have too many SStx.
		if isSStx && (numSStx >=
			int(g.cfg.ChainParams.MaxFreshStakePerBlock)) {
			log.Debugf("Skipping sstx %s because it would exceed "+
				"the max number of sstx allowed in a block", tx.Hash())
			logSkippedDeps(tx, deps)
			continue
		}

		// Skip if the SStx commit value is below the value required by the
		// stake diff.
		if isSStx && (tx.MsgTx().TxOut[0].Value < best.NextStakeDiff) {
			log.Debugf("Skipping ticket %s: price %d < stake diff %d", tx.Hash(), tx.MsgTx().TxOut[0].Value, best.NextStakeDiff)
			continue
		}

		// Skip revocation transactions if they are spending tickets that are not
		// eligible to be revoked.
		if isSSRtx {
			ticketHash := &tx.MsgTx().TxIn[0].PreviousOutPoint.Hash

			// Determine if the ticket being revoked will become missed or expired as
			// of this block.
			voted, winning := foundWinningTickets[*ticketHash]
			missedThisBlock := !voted && winning
			_, expiringThisBlock := expiringTicketHashes[*ticketHash]
			missedOrExpiredThisBlock := missedThisBlock || expiringThisBlock

			// Ensure the ticket being spent is actually eligible to be revoked in
			// this block.  If the automatic ticket revocations agenda is active, then
			// revocations are allowed if the ticket will become missed or expired as
			// of this block.
			eligible := (isAutoRevocationsEnabled && missedOrExpiredThisBlock) ||
				hashInSlice(*ticketHash, best.MissedTickets)

			if !eligible {
				log.Debugf("Skipping revocation %s: ticket %s not eligible", tx.Hash(), ticketHash)
				continue
			}
		}

		if miningView.isRejected(tx.Hash()) {
			// If the transaction or any of its ancestors have been rejected,
			// discard the transaction.
			log.Debugf("Skipping tx %s: rejected by ancestor check", tx.Hash())
			continue
		}

		ancestors := miningView.ancestors(tx.Hash())
		ancestorStats, _ := miningView.AncestorStats(tx.Hash())
		oldFee := prioItem.feePerKB

		// Recalculate fee using coin-type-aware method when available
		if g.cfg.FeeCalculator != nil {
			prioItem.feePerKB = calcCoinTypeAwareFeePerKb(prioItem.txDesc, ancestorStats, prioItem.coinType, g.cfg.FeeCalculator)
		} else {
			prioItem.feePerKB = calcFeePerKb(prioItem.txDesc, ancestorStats)
		}

		// Check if fee decreased (handle nil oldFee for auto-revocations etc)
		feeDecreased := oldFee != nil && prioItem.feePerKB != nil && oldFee.Cmp(prioItem.feePerKB) > 0
		if feeDecreased && ancestorStats.NumAncestors == 0 {
			// If the fee decreased due to ancestors being included in the
			// template and the transaction has no parents, then enqueue it one
			// more time with an accurate feePerKb.
			log.Debugf("Requeuing tx %s for fee recalc: %s -> %s (no ancestors)", tx.Hash(), oldFee, prioItem.feePerKB)
			heap.Push(priorityQueue, prioItem)
			prioritizedTxns[*tx.Hash()] = struct{}{}
			continue
		}

		if feeDecreased {
			// Skip the transaction if the total feePerKb decreased.
			// This addresses a vulnerability that would allow a low-fee
			// transaction to have an inflated and inaccurate feePerKb based on
			// ancestors that have already been included in the block template.
			// The transaction will be added back to the priority queue when all
			// parent transactions are included in the template.
			log.Debugf("Skipping tx %s: fee decreased %s -> %s (has %d ancestors)", tx.Hash(), oldFee, prioItem.feePerKB, ancestorStats.NumAncestors)
			continue
		}

		// Enforce coin type-based block space allocation.
		txSize := uint32(tx.MsgTx().SerializeSize())
		blockPlusTxSize := blockSize + txSize + uint32(ancestorStats.SizeBytes)

		// Check for arithmetic overflow
		if blockPlusTxSize < blockSize {
			log.Debugf("Skipping tx %s due to size arithmetic overflow", tx.Hash())
			logSkippedDeps(tx, deps)
			miningView.reject(tx.Hash())
			continue
		}

		// Check if transaction fits within coin type allocation
		// SKA emission transactions bypass this check because:
		// 1. They are the first transaction of their SKA type (always fit)
		// 2. They must be included at emission height (critical system transaction)
		// 3. Block space is guaranteed to be available for them
		coinType := blockalloc.GetTransactionCoinType(tx)
		isSKAEmission := wire.IsSKAEmissionTransaction(tx.MsgTx())

		if isSKAEmission {
			log.Infof("Including SKA emission tx %s (coin type %d, size %v) with guaranteed block space",
				tx.Hash(), coinType, txSize)
		} else if !transactionTracker.CanAddTransaction(tx) {
			log.Debugf("Skipping tx %s (coin type %d, size %v) because it "+
				"would exceed the coin type allocation; cur block "+
				"size %v, cur num tx %v", tx.Hash(), coinType, txSize,
				blockSize, len(blockTxns))
			logSkippedDeps(tx, deps)
			miningView.reject(tx.Hash())
			continue
		}

		// Enforce maximum signature operations per block.  Also check
		// for overflow.
		numSigOps := int64(prioItem.txDesc.TotalSigOps)
		numSigOpsBundle := numSigOps + int64(ancestorStats.TotalSigOps)
		if blockSigOps+numSigOpsBundle < blockSigOps ||
			blockSigOps+numSigOpsBundle > blockchain.MaxSigOpsPerBlock {
			log.Debugf("Skipping tx %s because it would "+
				"exceed the maximum sigops per block", tx.Hash())
			logSkippedDeps(tx, deps)
			miningView.reject(tx.Hash())
			continue
		}

		// Check to see if the SSGen tx actually uses a ticket that is
		// valid for the next block.
		if isSSGen {
			if foundWinningTickets[tx.MsgTx().TxIn[1].PreviousOutPoint.Hash] {
				log.Debugf("Skipping vote %s: already processed", tx.Hash())
				continue
			}
			msgTx := tx.MsgTx()
			isEligible := false
			for _, sstxHash := range best.NextWinningTickets {
				if sstxHash.IsEqual(&msgTx.TxIn[1].PreviousOutPoint.Hash) {
					isEligible = true
				}
			}

			if !isEligible {
				log.Debugf("Skipping vote %s: not eligible for next block", tx.Hash())
				continue
			}
		}

		// Reject transactions with insufficient fees, but use static minimum
		// fee (not dynamic multiplier) to avoid rejecting valid mempool
		// transactions when network conditions change. The priority queue
		// already handles fee-based prioritization for block space allocation.
		//
		// This protects against:
		// - 0-fee spam transactions
		// - Transactions from non-validating peers
		// - Direct RPC submissions bypassing mempool
		//
		// But allows valid mempool transactions (that passed dynamic fee
		// validation at entry time) to eventually be mined.
		skipForLowFee := false
		// Note: isSKAEmission already declared above for blockspace check
		if tx.Tree() != wire.TxTreeStake && !isSKAEmission {
			// Use coin-type-specific minimum relay fee
			var minStaticFeeBig *big.Int
			if prioItem.coinType == cointype.CoinTypeVAR {
				minStaticFeeBig = big.NewInt(int64(g.cfg.Policy.TxMinFreeFee))
			} else {
				// SKA coin types: look up fee from per-coin config
				if config, ok := g.cfg.ChainParams.SKACoins[prioItem.coinType]; ok && config.MinRelayTxFee != nil && config.MinRelayTxFee.Sign() > 0 {
					minStaticFeeBig = config.MinRelayTxFee
				} else {
					// Fallback default: 4 SKA per KB
					minStaticFeeBig = big.NewInt(4000000000000000000)
				}
			}
			// Apply 10% tolerance to account for integer division rounding in fee calculation.
			// Wallet calculates: fee = (feePerKB * txSize) / 1000 (truncates)
			// Miner recalculates: feePerKB = (fee * 1000) / txSize (can be slightly lower)
			// Example: 50 atoms/KB, 211 bytes → 10 atoms fee → 47.39 atoms/KB recalculated
			// minStaticFeeWithTolerance = minStaticFee * 90 / 100
			minStaticFeeWithTolerance := new(big.Int).Mul(minStaticFeeBig, big.NewInt(90))
			minStaticFeeWithTolerance.Div(minStaticFeeWithTolerance, big.NewInt(100))
			if prioItem.feePerKB.Cmp(minStaticFeeWithTolerance) < 0 {
				log.Debugf("Skipping tx %s with feePerKB %s < minimum %s (tolerance: %s)",
					tx.Hash(), prioItem.feePerKB, minStaticFeeBig, minStaticFeeWithTolerance)
				skipForLowFee = true
			}
		}

		if skipForLowFee {
			logSkippedDeps(tx, deps)
			miningView.reject(tx.Hash())
			continue
		}

		txBundle := append(ancestors, prioItem.txDesc)
		for _, bundledTx := range txBundle {
			// Ensure the transaction inputs pass all of the necessary
			// preconditions before allowing it to be added to the block.
			// The fraud proof is not checked because it will be filled in
			// by the miner.
			_, err = g.cfg.CheckTransactionInputs(bundledTx.Tx, nextBlockHeight,
				blockUtxos, false, &bestHeader, isTreasuryEnabled,
				isAutoRevocationsEnabled, subsidySplitVariant)
			if err != nil {
				log.Debugf("Skipping tx %s due to error in "+
					"CheckTransactionInputs: %v", bundledTx.Tx.Hash(), err)
				logSkippedDeps(bundledTx.Tx, deps)
				miningView.reject(bundledTx.Tx.Hash())
				continue nextPriorityQueueItem
			}
			// Skip standard script validation for SKA emission transactions.
			// SKA emissions use a special authorization format in the signature script
			// that doesn't follow standard script patterns. Instead, they are validated
			// through ValidateAuthorizedSKAEmissionTransaction which performs:
			// - Cryptographic signature verification binding to the transaction
			// - Admin key authorization checks
			// - Nonce-based replay protection
			// - Network ID verification
			// - Emission window and amount validation
			// This comprehensive validation happens during block validation, ensuring
			// SKA emissions are secure despite bypassing standard script checks.
			if !wire.IsSKAEmissionTransaction(bundledTx.Tx.MsgTx()) {
				err = g.cfg.ValidateTransactionScripts(bundledTx.Tx, blockUtxos,
					scriptFlags, isAutoRevocationsEnabled)
				if err != nil {
					log.Debugf("Skipping tx %s due to error in "+
						"ValidateTransactionScripts: %v", bundledTx.Tx.Hash(), err)
					logSkippedDeps(bundledTx.Tx, deps)
					miningView.reject(bundledTx.Tx.Hash())
					continue nextPriorityQueueItem
				}
			}
		}

		for _, bundledTxDesc := range txBundle {
			bundledTx := bundledTxDesc.Tx
			bundledTxHash := bundledTx.Hash()

			// Spend the transaction inputs in the block utxo view and add
			// an entry for it to ensure any transactions which reference
			// this one have it available as an input and can ensure they
			// aren't double spending.
			spendTransaction(blockUtxos, bundledTxDesc.Tx, nextBlockHeight,
				isTreasuryEnabled)

			// Add the transaction to the block, increment counters, and
			// save the fees and signature operation counts to the block
			// template.
			if wire.IsSKAEmissionTransaction(bundledTx.MsgTx()) {
				coinType := blockalloc.GetTransactionCoinType(bundledTx)
				maturityBlock := nextBlockHeight + int64(g.cfg.ChainParams.CoinbaseMaturity)
				log.Infof("Added SKA emission transaction %v (coin type %d) to block at height %d, matures at block %d",
					bundledTx.Hash(), coinType, nextBlockHeight, maturityBlock)
			} else {
				log.Debugf("Adding transaction %v to block at height %d", bundledTx.Hash(), nextBlockHeight)
			}
			blockTxns = append(blockTxns, bundledTx)
			blockSize += uint32(bundledTx.MsgTx().SerializeSize())
			bundledTxSigOps := int64(bundledTxDesc.TotalSigOps)
			blockSigOps += bundledTxSigOps

			// Update block space allocation tracking
			transactionTracker.AddTransaction(bundledTx)

			// Fee recording for this tx happened at mempool acceptance; recording
			// again here would over-weight self-mined txs on high-hashrate nodes
			// (the asymmetry that previously poisoned testnode1's median).

			// Accumulate the SStxs in the block, because only a certain number
			// are allowed.
			if bundledTxDesc.Type == stake.TxTypeSStx {
				numSStx++
			}
			if bundledTxDesc.Type == stake.TxTypeSSGen {
				foundWinningTickets[bundledTx.MsgTx().TxIn[1].PreviousOutPoint.Hash] = true
				numSSGen++
			}
			if isTreasuryEnabled && bundledTxDesc.Type == stake.TxTypeTAdd {
				numTAdds++
			}

			templateTxnMap[*bundledTxHash] = struct{}{}
			// Store fee as big.Int (use SKAFee for SKA, Fee for VAR)
			if blockalloc.GetTransactionCoinType(bundledTx).IsSKA() && bundledTxDesc.SKAFee != nil {
				txFeesMap[*bundledTxHash] = new(big.Int).Set(bundledTxDesc.SKAFee)
			} else {
				txFeesMap[*bundledTxHash] = big.NewInt(bundledTxDesc.Fee)
			}
			txSigOpCountsMap[*bundledTxHash] = bundledTxSigOps

			bundledPrioItem := prioItemMap[*bundledTxHash]
			log.Tracef("Adding tx %s (priority %.2f, feePerKB %.2f)",
				bundledTxHash, bundledPrioItem.priority,
				bundledPrioItem.feePerKB)

			// Remove transaction from mining view since it's been added to the
			// block template.
			bundledTxDeps := miningView.children(bundledTxHash)
			miningView.RemoveTransaction(bundledTxHash, false)
			log.Debugf("Removed transaction %v from mining view, priority queue length now: %d", bundledTxHash, priorityQueue.Len())

			// Add transactions which depend on this one (and also do not
			// have any other unsatisfied dependencies) to the priority
			// queue.
			for _, childTx := range bundledTxDeps {
				childTxHash := childTx.Tx.Hash()
				if _, exist := prioritizedTxns[*childTxHash]; exist {
					continue
				}

				// Add the transaction to the priority queue if there are no
				// more dependencies after this one and the priority item for it
				// already exists.
				if !miningView.hasParents(childTxHash) {
					childPrioItem := prioItemMap[*childTxHash]
					if childPrioItem != nil {
						heap.Push(priorityQueue, childPrioItem)
						prioritizedTxns[*childTxHash] = struct{}{}
					}
				}
			}
		}
	}

	// If we did not already add auto revocations above, attempt to add them now.
	// This case will be hit if the priority queue contained only vote
	// transactions.
	if isAutoRevocationsEnabled && !addedAutoRevocations {
		// Create revocations for all tickets that will become missed or expired
		// as of the block being created and add them to the priority queue and
		// maps.
		err = g.addAutoRevocationsToQueue(foundWinningTickets, blockUtxos,
			bestHeaderBytes, numSSGen, isTreasuryEnabled, priorityQueue,
			prioritizedTxns, prioItemMap)
		if err != nil {
			return nil, err
		}

		// Set the flag that auto revocations have been added so that they are
		// only added once.
		addedAutoRevocations = true

		// Continue to dequeue from the priority queue now that the revocations have
		// been added.
		goto nextPriorityQueueItem
	}

	// Log block space allocation results and update fee calculator with utilization data
	allocation := transactionTracker.GetAllocation()
	log.Debugf("Block space allocation: %.1f%% utilization (%d/%d bytes used)",
		allocation.GetUtilizationPercentage(), allocation.TotalUsed, allocation.TotalAllocated)

	// Pre-bucket pending transactions by coin type for performance (O(n) instead of O(n*m))
	var pendingBuckets map[cointype.CoinType]struct {
		count int
		size  int64
	}
	if g.cfg.FeeCalculator != nil {
		pendingBuckets = make(map[cointype.CoinType]struct {
			count int
			size  int64
		})
		pendingTxs := miningView.TxDescs()
		for _, txDesc := range pendingTxs {
			coinType := blockalloc.GetTransactionCoinType(txDesc.Tx)
			bucket := pendingBuckets[coinType]
			bucket.count++
			bucket.size += int64(txDesc.Tx.MsgTx().SerializeSize())
			pendingBuckets[coinType] = bucket
		}
	}

	for coinType, coinAlloc := range allocation.Allocations {
		if coinAlloc.UsedBytes > 0 {
			log.Debugf("  Coin type %d: %d bytes used (%.1f%% of allocation)",
				coinType, coinAlloc.UsedBytes,
				float64(coinAlloc.UsedBytes)/float64(coinAlloc.FinalAllocation)*100.0)
		}

		// Update fee calculator with utilization feedback for dynamic adjustment
		if g.cfg.FeeCalculator != nil {
			var utilizationRate float64
			if coinAlloc.FinalAllocation > 0 {
				utilizationRate = float64(coinAlloc.UsedBytes) / float64(coinAlloc.FinalAllocation)
			}
			// utilizationRate is 0.0 if allocation is 0 (no space allocated)

			bucket := pendingBuckets[coinType]
			g.cfg.FeeCalculator.UpdateUtilization(coinType,
				bucket.count, bucket.size, utilizationRate)
		}
	}

	// Build tx list for stake tx.
	blockTxnsStake := make([]*dcrutil.Tx, 0, len(blockTxns))

	// Create a standard coinbase transaction paying to the provided
	// address.  NOTE: The coinbase value will be updated to include the
	// fees from the selected transactions later after they have actually
	// been selected.  It is created here to detect any errors early
	// before potentially doing a lot of work below.  The extra nonce helps
	// ensure the transaction is not a duplicate transaction (paying the
	// same value to the same public key address would otherwise be an
	// identical transaction for block version 1).
	// Decred: We need to move this downwards because of the requirements
	// to incorporate voters and potential voters.
	//
	// NOTE: we have to do this early to deal with stakebase.
	coinbaseScript := []byte{0x00, 0x00}
	coinbaseScript = append(coinbaseScript, []byte(coinbaseFlags)...)

	// Add a random coinbase nonce to ensure that tx prefix hash
	// so that our merkle root is unique for lookups needed for
	// getwork, etc.
	opReturnPkScript, err := standardCoinbaseOpReturn(uint32(nextBlockHeight))
	if err != nil {
		return nil, err
	}

	// Stake tx ordering in stake tree:
	// 1. Stakebase/TreasuryBase (if treasury enabled)
	// 2. SSGen (votes)
	// 3. SSFee (non-VAR fee distributions to stakers)
	// 4. SStx (fresh stake tickets)
	// 5. SSRtx (revocations for missed tickets)
	// 6. TAdd/TSpend (treasury operations if enabled)

	// We have to figure out how many voters we have before adding the
	// stakebase transaction.
	//
	// We should only find at most TicketsPerBlock votes per block, so
	// initialize the slices using that as a hint to avoid reallocations.
	voters := 0
	voteBitsVoters := make([]uint16, 0, g.cfg.ChainParams.TicketsPerBlock)
	votes := make([]*dcrutil.Tx, 0, g.cfg.ChainParams.TicketsPerBlock)

	// After SVH, we should add SSGens (votes) to the stake tree. Since we
	// need to figure out the number of voters before creating the
	// treasuryBase, we add the votes to an aux slice to be processed
	// later.
	if nextBlockHeight >= stakeValidationHeight {
		for _, tx := range blockTxns {
			msgTx := tx.MsgTx()

			if stake.IsSSGen(msgTx) {
				txCopy := dcrutil.NewTxDeepTxIns(tx)
				if g.maybeInsertStakeTx(txCopy, !knownDisapproved,
					isTreasuryEnabled) {

					vb := stake.SSGenVoteBits(txCopy.MsgTx())
					voteBitsVoters = append(voteBitsVoters, vb)
					votes = append(votes, txCopy)
					voters++
				}
			}

			// Don't let this overflow, although probably it's impossible.
			if voters >= math.MaxUint16 {
				break
			}
		}
	}

	// If the treasury is enabled, it should be the first tx of the stake
	// tree.
	var treasuryBase *dcrutil.Tx
	if isTreasuryEnabled {
		treasuryBase, err = createTreasuryBaseTx(g.cfg.SubsidyCache,
			nextBlockHeight, uint16(voters))
		if err != nil {
			return nil, err
		}
		blockTxnsStake = append(blockTxnsStake, treasuryBase)
	}

	// Add the votes to blockTxnsStake since treasuryBase needs to come
	// first.
	blockTxnsStake = append(blockTxnsStake, votes...)

	// Create SSFee transactions for non-VAR fees if we have voters
	// This must happen after votes are added but before fresh stake tickets
	var ssFeeTxns []*dcrutil.Tx
	if nextBlockHeight >= stakeValidationHeight && voters > 0 {
		// We'll calculate the fee split after all regular transactions are processed
		// For now, just track that we need to create SSFee transactions
		// The actual creation will happen after totalFees is calculated
	}

	// Set votebits, which determines whether the TxTreeRegular of the previous
	// block is valid or not.
	var votebits uint16
	if nextBlockHeight < stakeValidationHeight {
		votebits = uint16(0x0001) // TxTreeRegular enabled pre-staking
	} else {
		// Otherwise, we need to check the votes to determine if the tx tree was
		// validated or not.
		voteYea := 0
		totalVotes := 0

		for _, vb := range voteBitsVoters {
			if dcrutil.IsFlagSet16(vb, dcrutil.BlockValid) {
				voteYea++
			}
			totalVotes++
		}

		if voteYea == 0 { // Handle zero case for div by zero error prevention.
			votebits = uint16(0x0000) // TxTreeRegular disabled
		} else if (totalVotes / voteYea) <= 1 {
			votebits = uint16(0x0001) // TxTreeRegular enabled
		} else {
			votebits = uint16(0x0000) // TxTreeRegular disabled
		}
	}

	// Get the newly purchased tickets (SStx tx) and store them and their
	// number.
	freshStake := 0
	for _, tx := range blockTxns {
		msgTx := tx.MsgTx()
		if tx.Tree() == wire.TxTreeStake && stake.IsSStx(msgTx) {
			// A ticket can not spend an input from TxTreeRegular,
			// since it has not yet been validated.
			if containsTxIns(blockTxns, tx) {
				continue
			}

			// Quick check for difficulty here.
			if msgTx.TxOut[0].Value >= best.NextStakeDiff {
				txCopy := dcrutil.NewTxDeepTxIns(tx)
				if g.maybeInsertStakeTx(txCopy, !knownDisapproved,
					isTreasuryEnabled) {

					blockTxnsStake = append(blockTxnsStake, txCopy)
					freshStake++
				}
			}
		}

		// Don't let this overflow.
		if freshStake >= int(g.cfg.ChainParams.MaxFreshStakePerBlock) {
			break
		}
	}

	// Check if there are staged tickets waiting for their parent split
	// transactions to be confirmed. This is used by the exhaustion check
	// to allow mining when staged tickets exist (they'll be available next block).
	hasStagedTickets := g.cfg.TxSource.HasStagedTicketsWithMempoolParent()

	// Ensure that mining the block would not cause the chain to become
	// unrecoverable due to ticket exhaustion.
	err = g.cfg.CheckTicketExhaustion(&best.Hash, uint8(freshStake), hasStagedTickets)
	if err != nil {
		log.Debug(err)
		return nil, makeError(ErrTicketExhaustion, err.Error())
	}

	// Get the ticket revocations (SSRtx tx) and store them and their
	// number.
	revocations := 0
	for _, tx := range blockTxns {
		if nextBlockHeight < stakeValidationHeight {
			break // No SSRtx should be present before this height.
		}

		msgTx := tx.MsgTx()
		if tx.Tree() == wire.TxTreeStake && stake.IsSSRtx(msgTx) {
			txCopy := dcrutil.NewTxDeepTxIns(tx)
			if g.maybeInsertStakeTx(txCopy, !knownDisapproved,
				isTreasuryEnabled) {

				blockTxnsStake = append(blockTxnsStake, txCopy)
				revocations++
			}
		}

		// Don't let this overflow.
		if revocations >= math.MaxUint8 {
			break
		}
	}

	// Insert TAdd/TSpend transactions.
	if isTreasuryEnabled {
		for _, tx := range blockTxns {
			msgTx := tx.MsgTx()
			if tx.Tree() == wire.TxTreeStake && stake.IsTAdd(msgTx) {
				txCopy := dcrutil.NewTxDeepTxIns(tx)
				if g.maybeInsertStakeTx(txCopy, !knownDisapproved,
					isTreasuryEnabled) {

					blockTxnsStake = append(blockTxnsStake, txCopy)
					log.Tracef("maybeInsertStakeTx TADD %v ", tx.Hash())
				}
			} else if tx.Tree() == wire.TxTreeStake && stake.IsTSpend(msgTx) {
				txCopy := dcrutil.NewTxDeepTxIns(tx)
				if g.maybeInsertStakeTx(txCopy, !knownDisapproved,
					isTreasuryEnabled) {

					blockTxnsStake = append(blockTxnsStake, txCopy)
					log.Tracef("maybeInsertStakeTx TSPEND %v ", tx.Hash())
				}
			}
		}
	}

	coinbaseTx := createCoinbaseTx(g.cfg.SubsidyCache, coinbaseScript,
		opReturnPkScript, nextBlockHeight, payToAddress, uint16(voters),
		g.cfg.ChainParams, isTreasuryEnabled, subsidySplitVariant)
	coinbaseTx.SetTree(wire.TxTreeRegular)

	numCoinbaseSigOps := int64(g.cfg.CountSigOps(coinbaseTx, true,
		false, isTreasuryEnabled))
	blockSize += uint32(coinbaseTx.MsgTx().SerializeSize())
	blockSigOps += numCoinbaseSigOps
	txFeesMap[*coinbaseTx.Hash()] = big.NewInt(0)
	txSigOpCountsMap[*coinbaseTx.Hash()] = numCoinbaseSigOps
	if treasuryBase != nil {
		txFeesMap[*treasuryBase.Hash()] = big.NewInt(0)
		n := int64(g.cfg.CountSigOps(treasuryBase, true, false, isTreasuryEnabled))
		txSigOpCountsMap[*treasuryBase.Hash()] = n
	}

	// Build tx lists for regular tx.
	blockTxnsRegular := make([]*dcrutil.Tx, 0, len(blockTxns)+1)

	// Append coinbase.
	blockTxnsRegular = append(blockTxnsRegular, coinbaseTx)

	// Assemble the two transaction trees.
	for _, tx := range blockTxns {
		if tx.Tree() == wire.TxTreeRegular {
			blockTxnsRegular = append(blockTxnsRegular, tx)
		} else if tx.Tree() == wire.TxTreeStake {
			continue
		} else {
			log.Tracef("Error adding tx %s to block; invalid tree", tx.Hash())
			continue
		}
	}

	for _, tx := range blockTxnsRegular {
		fee, ok := txFeesMap[*tx.Hash()]
		if !ok {
			return nil, fmt.Errorf("couldn't find fee for tx %v",
				*tx.Hash())
		}

		// Determine coin type for this transaction and add fees by type
		coinType := blockalloc.GetTransactionCoinType(tx)
		totalFees.AddBig(coinType, fee)
		txFees = append(txFees, fee)

		tsos, ok := txSigOpCountsMap[*tx.Hash()]
		if !ok {
			return nil, fmt.Errorf("couldn't find sig ops count for tx %v",
				*tx.Hash())
		}
		txSigOpCounts = append(txSigOpCounts, tsos)
	}

	for _, tx := range blockTxnsStake {
		fee, ok := txFeesMap[*tx.Hash()]
		if !ok {
			return nil, fmt.Errorf("couldn't find fee for stx %v",
				*tx.Hash())
		}

		// Determine coin type for this transaction and add fees by type
		coinType := blockalloc.GetTransactionCoinType(tx)
		totalFees.AddBig(coinType, fee)
		txFees = append(txFees, fee)

		tsos, ok := txSigOpCountsMap[*tx.Hash()]
		if !ok {
			return nil, fmt.Errorf("couldn't find sig ops count for stx %v",
				*tx.Hash())
		}
		txSigOpCounts = append(txSigOpCounts, tsos)
	}

	// Distribute fees once stake validation height is reached.
	// VAR fees are scaled by voters/TicketsPerBlock so that missing voters
	// cause the unclaimed VAR share to burn (matches the VAR fee scaling of
	// the validator in blockchain checkTransactionsAndConnect for the coinbase
	// and checkConnectBlock for SSFee transactions).
	// SKA fees are distributed in full to the voters that actually voted.
	// Track miner's VAR fee share for coinbase (captured early to prevent loss)
	var minerVARFeeForCoinbase int64

	if nextBlockHeight >= stakeValidationHeight {
		// Scale VAR fees by voter participation to mirror validator's fee-burn
		// behavior. When voters < TicketsPerBlock, the
		// unclaimed VAR share is burned. SKA fees are distributed in full.
		bigVoters := new(big.Int).SetInt64(int64(voters))
		bigTickets := new(big.Int).SetInt64(int64(g.cfg.ChainParams.TicketsPerBlock))
		for coinType, fee := range totalFees {
			if fee == nil || fee.Sign() <= 0 {
				continue
			}
			if coinType.IsSKA() {
				continue
			}
			scaledBig := new(big.Int).Mul(fee, bigVoters)
			scaledBig.Div(scaledBig, bigTickets)
			totalFees[coinType] = scaledBig
		}

		// Get subsidy proportions for fee splitting
		work, stake, _, _ := standalone.GetSubsidyProportions(subsidySplitVariant)

		// Calculate fee split between miners and stakers
		minerFees, stakerFees := wire.CalcFeeSplitByCoinType(totalFees, work, stake)

		// Capture miner's VAR fee immediately to ensure it's not lost
		// This value will be added to coinbase output later
		minerVARFeeForCoinbase = minerFees.Get(cointype.CoinTypeVAR)
		if minerVARFeeForCoinbase > 0 {
			log.Debugf("Block %d: miner VAR fee share = %d atoms", nextBlockHeight, minerVARFeeForCoinbase)
		}

		// Create SSFee transactions for staker fees if there are voters
		if voters > 0 {
			// Helper function to create staker SSFee transactions for a coin type
			createStakerSSFee := func(coinType cointype.CoinType, stakerFee *big.Int) error {
				// Create batched SSFee transactions for this coin type (one per consolidation address)
				// Use SSFeeIndex for UTXO augmentation if available
				// Note: Both VAR and non-VAR staker fees use SSFee (only VAR miner fees go to coinbase)
				voterSSFeeTxns, err := createSSFeeTxBatched(coinType, stakerFee, votes, nextBlockHeight,
					g.cfg.SSFeeIndex, blockUtxos, g.cfg.FetchUtxoEntry, g)
				if err != nil {
					// Critical error: staker fees cannot be distributed
					// This is a serious issue as fees would be lost if we continue
					return fmt.Errorf("failed to create staker SSFee txs for coin type %d (amount: %v): %w",
						coinType, stakerFee, err)
				}

				// Add all voter SSFee transactions to stake tree
				for _, ssFeeTx := range voterSSFeeTxns {
					ssFeeTxns = append(ssFeeTxns, ssFeeTx)
					blockTxnsStake = append(blockTxnsStake, ssFeeTx)

					// Update block size
					blockSize += uint32(ssFeeTx.MsgTx().SerializeSize())

					// Track for fee accounting (SSFee has no input fees)
					txFeesMap[*ssFeeTx.Hash()] = big.NewInt(0)
					txSigOpCountsMap[*ssFeeTx.Hash()] = 0

					// Update the UTXO view with SSFee transaction outputs so they are
					// available for validation and future augmentation
					spendTransaction(blockUtxos, ssFeeTx, nextBlockHeight, isTreasuryEnabled)
				}

				log.Debugf("Created %d SSFee txs for coin type %d, distributing %d total to %d voters",
					len(voterSSFeeTxns), coinType, stakerFee, voters)
				return nil
			}

			// Process all staker fees (all stored as *big.Int)
			for coinType, stakerFee := range stakerFees {
				if stakerFee == nil || stakerFee.Sign() <= 0 {
					continue
				}
				if err := createStakerSSFee(coinType, stakerFee); err != nil {
					return nil, err
				}
			}
		}

		// Create miner SSFee transactions for non-VAR (SKA) miner fees
		// This distributes non-VAR fees to miners through SSFee transactions
		// instead of adding them to the coinbase, maintaining coin type separation
		// Note: VAR miner fees go to coinbase, only SKA miner fees use SSFee
		for coinType, minerFee := range minerFees {
			// Skip VAR - VAR miner fees go to coinbase
			if coinType.IsVAR() || minerFee == nil || minerFee.Sign() <= 0 {
				continue
			}

			// Create miner SSFee transaction for this coin type
			// Uses SSFeeIndex to find existing miner SSFee UTXOs for consolidation
			minerSSFeeTx, err := createMinerSSFeeTx(coinType, minerFee, payToAddress, nextBlockHeight, g.cfg.SSFeeIndex, blockUtxos, g.cfg.FetchUtxoEntry, g)
			if err != nil {
				// Critical error: miner fees cannot be distributed
				// This is a serious issue as fees would be lost if we continue
				return nil, fmt.Errorf("failed to create miner SSFee tx for coin type %d (amount: %v): %w",
					coinType, minerFee, err)
			}

			// Add to stake tree transactions
			ssFeeTxns = append(ssFeeTxns, minerSSFeeTx)
			blockTxnsStake = append(blockTxnsStake, minerSSFeeTx)

			// Update block size
			blockSize += uint32(minerSSFeeTx.MsgTx().SerializeSize())

			// Track for fee accounting (SSFee has no input fees)
			txFeesMap[*minerSSFeeTx.Hash()] = big.NewInt(0)
			txSigOpCountsMap[*minerSSFeeTx.Hash()] = 0

			// Update the UTXO view with miner SSFee transaction outputs so they are
			// available for validation and future augmentation
			spendTransaction(blockUtxos, minerSSFeeTx, nextBlockHeight, isTreasuryEnabled)

		}

		// Update totalFees to only include VAR miner fees for the coinbase
		// All non-VAR fees have been distributed via SSFee transactions
		// Note: minerVARFeeForCoinbase was captured at the start of this block
		totalFees = wire.NewFeesByType()
		totalFees.Add(cointype.CoinTypeVAR, minerVARFeeForCoinbase)
	}

	txSigOpCounts = append(txSigOpCounts, numCoinbaseSigOps)

	// Now that the actual transactions have been selected, update the
	// block size for the real transaction count and coinbase value with
	// the total fees accordingly.
	if nextBlockHeight > 1 {
		blockSize -= wire.MaxVarIntPayload -
			uint32(wire.VarIntSerializeSize(uint64(len(blockTxnsRegular))+
				uint64(len(blockTxnsStake))))
		powOutputIdx := 2
		if isTreasuryEnabled {
			powOutputIdx = 1
		}
		// Add VAR fees to coinbase - only VAR fees go to the PoW output
		// Non-VAR fees have already been distributed via miner SSFee transactions
		varFees := totalFees.Get(cointype.CoinTypeVAR)
		if varFees > 0 {
			coinbaseTx.MsgTx().TxOut[powOutputIdx].Value += varFees
		}
		// Use varFees directly for accounting consistency - this is exactly what was added to coinbase
		txFees[0] = big.NewInt(-varFees)
	}

	// Calculate the required difficulty for the block.  The timestamp
	// is potentially adjusted to ensure it comes after the median time of
	// the last several blocks per the chain consensus rules.
	ts := g.medianAdjustedTime()
	reqDifficulty, err := g.cfg.CalcNextRequiredDifficulty(&prevHash, ts)
	if err != nil {
		return nil, makeError(ErrGettingDifficulty, err.Error())
	}

	// Return nil if we don't yet have enough voters; sometimes it takes a
	// bit for the mempool to sync with the votes map and we end up down
	// here despite having the relevant votes available in the votes map.
	minimumVotesRequired :=
		int((g.cfg.ChainParams.TicketsPerBlock / 2) + 1)
	if nextBlockHeight >= stakeValidationHeight &&
		voters < minimumVotesRequired {
		log.Warnf("incongruent number of voters in mempool " +
			"vs mempool.voters; not enough voters found")
		return g.handleTooFewVoters(nextBlockHeight, payToAddress,
			isTreasuryEnabled, subsidySplitVariant)
	}

	// Correct transaction index fraud proofs for any transactions that
	// are chains. maybeInsertStakeTx fills this in for stake transactions
	// already, so only do it for regular transactions.
	for i, tx := range blockTxnsRegular {
		// No need to check any of the transactions in the custom first
		// block.
		if nextBlockHeight == 1 {
			break
		}

		// Skip coinbase.
		if i == 0 {
			continue
		}

		view, err := g.cfg.FetchUtxoView(tx, !knownDisapproved)
		if err != nil {
			str := fmt.Sprintf("failed to fetch utxo view for tx %v: %s",
				tx.Hash(), err.Error())
			return nil, makeError(ErrFetchTxStore, str)
		}

		// Copy the transaction and swap the pointer.
		txCopy := dcrutil.NewTxDeepTxIns(tx)
		blockTxnsRegular[i] = txCopy
		tx = txCopy

		for _, txIn := range tx.MsgTx().TxIn {
			entry := view.LookupEntry(txIn.PreviousOutPoint)
			if entry == nil {
				// Set a flag with the index so we can properly set
				// the fraud proof below.
				txIn.BlockIndex = wire.NullBlockIndex
			} else {
				txIn.ValueIn = entry.Amount()
				txIn.SKAValueIn = entry.SKAAmount() // For SKA UTXOs
				txIn.BlockHeight = uint32(entry.BlockHeight())
				txIn.BlockIndex = entry.BlockIndex()
			}
		}
	}

	// Fill in locally referenced inputs.
	for i, tx := range blockTxnsRegular {
		// Skip coinbase and SKA emission
		if i == 0 || wire.IsSKAEmissionTransaction(tx.MsgTx()) {
			continue
		}

		for _, txIn := range tx.MsgTx().TxIn {
			// This tx was at some point 0-conf and now requires the correct block
			// height and index.  Set it here.  It is safe to modify the transaction
			// directly since it has already been copied above.
			if txIn.BlockIndex == wire.NullBlockIndex {
				idx := txIndexFromTxList(txIn.PreviousOutPoint.Hash,
					blockTxnsRegular)

				// The input is in the block, set it accordingly.
				if idx != -1 {
					originIdx := txIn.PreviousOutPoint.Index
					originTxOut := blockTxnsRegular[idx].MsgTx().TxOut[originIdx]
					txIn.ValueIn = originTxOut.Value
					txIn.SKAValueIn = originTxOut.SKAValue // For SKA outputs
					txIn.BlockHeight = uint32(nextBlockHeight)
					txIn.BlockIndex = uint32(idx)
				} else {
					str := fmt.Sprintf("failed find hash in tx list "+
						"for fraud proof; tx in hash %v",
						txIn.PreviousOutPoint.Hash)
					return nil, makeError(ErrFraudProofIndex, str)
				}
			}
		}
	}

	// Choose the block version to generate based on the network.
	blockVersion := int32(generatedBlockVersion)
	if g.cfg.ChainParams.Net != wire.MainNet {
		blockVersion = generatedBlockVersionTest
	}

	// Figure out stake version.
	generatedStakeVersion, err := g.cfg.CalcStakeVersionByHash(&prevHash)
	if err != nil {
		return nil, err
	}

	// Create a new block ready to be solved.
	var msgBlock wire.MsgBlock
	msgBlock.Header = wire.BlockHeader{
		Version:   blockVersion,
		PrevBlock: prevHash,
		// MerkleRoot and StakeRoot set below.
		VoteBits:     votebits,
		FinalState:   best.NextFinalState,
		Voters:       uint16(voters),
		FreshStake:   uint8(freshStake),
		Revocations:  uint8(revocations),
		PoolSize:     best.NextPoolSize,
		Timestamp:    ts,
		SBits:        best.NextStakeDiff,
		Bits:         reqDifficulty,
		StakeVersion: generatedStakeVersion,
		Height:       uint32(nextBlockHeight),
		// Size declared below
	}

	for _, tx := range blockTxnsRegular {
		if err := msgBlock.AddTransaction(tx.MsgTx()); err != nil {
			return nil, makeError(ErrTransactionAppend, err.Error())
		}
	}

	totalTreasuryOps := 0
	for _, tx := range blockTxnsStake {
		if err := msgBlock.AddSTransaction(tx.MsgTx()); err != nil {
			return nil, makeError(ErrTransactionAppend, err.Error())
		}
		// While in this loop count treasury operations.
		if isTreasuryEnabled {
			if stake.IsTAdd(tx.MsgTx()) {
				totalTreasuryOps++
			} else if stake.IsTSpend(tx.MsgTx()) {
				totalTreasuryOps++
			}
		}
	}

	// Calculate the merkle root depending on the result of the header
	// commitments agenda vote.
	hdrCmtActive, err := g.cfg.IsHeaderCommitmentsAgendaActive(&prevHash)
	if err != nil {
		return nil, err
	}
	calculatedMerkleRoot := calcBlockMerkleRoot(msgBlock.Transactions,
		msgBlock.STransactions, hdrCmtActive)
	msgBlock.Header.MerkleRoot = calculatedMerkleRoot

	// Calculate the stake root or commitment root depending on the result of
	// the header commitments agenda vote.
	var cmtRoot chainhash.Hash
	if hdrCmtActive {
		cmtRoot, err = calcBlockCommitmentRootV1(&msgBlock, blockUtxos)
		if err != nil {
			str := fmt.Sprintf("failed to calculate commitment root for block "+
				"when making new block template: %v", err)
			return nil, makeError(ErrCalcCommitmentRoot, str)
		}
	} else {
		cmtRoot = standalone.CalcTxTreeMerkleRoot(msgBlock.STransactions)
	}
	msgBlock.Header.StakeRoot = cmtRoot

	msgBlock.Header.Size = uint32(msgBlock.SerializeSize())

	// Finally, perform a full check on the created block against the chain
	// consensus rules to ensure it properly connects to the current best
	// chain with no issues.
	block := dcrutil.NewBlockDeepCopyCoinbase(&msgBlock)
	err = g.cfg.CheckConnectBlockTemplate(block)
	if err != nil {
		str := fmt.Sprintf("failed to do final check for check connect "+
			"block when making new block template: %v", err)
		log.Debugf(str)
		return nil, makeError(ErrCheckConnectBlock, str)
	}

	log.Debugf("Created new block template (%d transactions, %d stake "+
		"transactions (%d SSFee), %d treasury transactions, %d VAR fees, %d signature "+
		"operations, %d bytes, target difficulty %064x, stake difficulty %v)",
		len(msgBlock.Transactions), len(msgBlock.STransactions),
		len(ssFeeTxns), totalTreasuryOps, totalFees.Get(cointype.CoinTypeVAR), blockSigOps, blockSize,
		standalone.CompactToBig(msgBlock.Header.Bits),
		dcrutil.Amount(msgBlock.Header.SBits).ToCoin())

	blockTemplate := &BlockTemplate{
		Block:           &msgBlock,
		Fees:            txFees,
		SigOpCounts:     txSigOpCounts,
		Height:          nextBlockHeight,
		ValidPayAddress: payToAddress != nil,
	}

	return blockTemplate, nil
}

// UpdateBlockTime updates the timestamp in the passed header to the current
// time while taking into account the median time of the last several blocks to
// ensure the new time is after that time per the chain consensus rules.
func (g *BlkTmplGenerator) UpdateBlockTime(header *wire.BlockHeader) {
	// The new timestamp is potentially adjusted to ensure it comes after
	// the median time of the last several blocks per the chain consensus
	// rules.
	newTimestamp := g.medianAdjustedTime()
	header.Timestamp = newTimestamp
}
