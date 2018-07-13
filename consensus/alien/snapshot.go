// Copyright 2017 The gttc Authors
// This file is part of the gttc library.
//
// The gttc library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The gttc library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the gttc library. If not, see <http://www.gnu.org/licenses/>.

// Package alien implements the delegated-proof-of-stake consensus engine.

package alien

import (
	"encoding/json"
	"math/big"
	"math/rand"
	"sort"
	"time"

	"github.com/TTCECO/gttc/common"
	"github.com/TTCECO/gttc/core/types"
	"github.com/TTCECO/gttc/ethdb"
	"github.com/TTCECO/gttc/params"
	"github.com/TTCECO/gttc/rlp"
	"github.com/hashicorp/golang-lru"
)

const (
	defaultFullCredit 	= 1000				// no punished
	missingPublishCredit = 100				// punished for missing one block seal
	signRewardCredit	= 10				// seal one block
	minCalSignerQueueCredit = 300			// when calculate the signerQueue,
											// the credit of one signer is at least minCalSignerQueueCredit
)
// Snapshot is the state of the authorization voting at a given point in time.
type Snapshot struct {
	config   *params.AlienConfig // Consensus engine parameters to fine tune behavior
	sigcache *lru.ARCCache       // Cache of recent block signatures to speed up ecrecover

	Number uint64      `json:"number"` // Block number where the snapshot was created
	Hash   common.Hash `json:"hash"`   // Block hash where the snapshot was created

	Signers []*common.Address `json:"signers"` // Signers queue in current header
	// The signer validate should judge by last snapshot
	Votes  map[common.Address]*Vote    `json:"votes"`  // All validate votes from genesis block
	Tally  map[common.Address]*big.Int `json:"tally"`  // Stake for each candidate address
	Voters map[common.Address]*big.Int `json:"voters"` // block number for each voter address
	Punished map[common.Address] uint64 `json:"punished"` // The signer be punished count cause of missing seal

	HeaderTime    uint64 `json:"headerTime"`    // Time of the current header
	LoopStartTime uint64 `json:"loopStartTime"` // Start Time of the current loop

}

// newSnapshot creates a new snapshot with the specified startup parameters. only ever use if for
// the genesis block.
func newSnapshot(config *params.AlienConfig, sigcache *lru.ARCCache, hash common.Hash, votes []*Vote) *Snapshot {
	snap := &Snapshot{
		config:        config,
		sigcache:      sigcache,
		Number:        0,
		Hash:          hash,
		Signers:       []*common.Address{},
		Votes:         make(map[common.Address]*Vote),
		Tally:         make(map[common.Address]*big.Int),
		Voters:        make(map[common.Address]*big.Int),
		Punished:		make(map[common.Address]uint64),
		HeaderTime:    uint64(time.Now().Unix()) - 1,//config.GenesisTimestamp - 1, //
		LoopStartTime: config.GenesisTimestamp,
	}

	for _, vote := range votes {
		// init Votes from each vote
		snap.Votes[vote.Voter] = vote

		// init Tally
		_, ok := snap.Tally[vote.Candidate]
		if !ok {
			snap.Tally[vote.Candidate] = big.NewInt(0)
		}
		snap.Tally[vote.Candidate].Add(snap.Tally[vote.Candidate], vote.Stake)

		// init Voters
		snap.Voters[vote.Voter] = big.NewInt(0) // block number is 0 , vote in genesis block

	}

	for i := 0; i < int(config.MaxSignerCount); i++ {
		snap.Signers = append(snap.Signers, &config.SelfVoteSigners[i % len(config.SelfVoteSigners)])
	}

	return snap
}

// loadSnapshot loads an existing snapshot from the database.
func loadSnapshot(config *params.AlienConfig, sigcache *lru.ARCCache, db ethdb.Database, hash common.Hash) (*Snapshot, error) {
	blob, err := db.Get(append([]byte("alien-"), hash[:]...))
	if err != nil {
		return nil, err
	}
	snap := new(Snapshot)
	if err := json.Unmarshal(blob, snap); err != nil {
		return nil, err
	}
	snap.config = config
	snap.sigcache = sigcache

	return snap, nil
}

// store inserts the snapshot into the database.
func (s *Snapshot) store(db ethdb.Database) error {
	blob, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return db.Put(append([]byte("alien-"), s.Hash[:]...), blob)
}

// copy creates a deep copy of the snapshot, though not the individual votes.
func (s *Snapshot) copy() *Snapshot {
	cpy := &Snapshot{
		config:   s.config,
		sigcache: s.sigcache,
		Number:   s.Number,
		Hash:     s.Hash,

		Signers: make([]*common.Address, len(s.Signers)),
		Votes:   make(map[common.Address]*Vote),
		Tally:   make(map[common.Address]*big.Int),
		Voters:  make(map[common.Address]*big.Int),
		Punished:make(map[common.Address]uint64),

		HeaderTime:    s.HeaderTime,
		LoopStartTime: s.LoopStartTime,
	}
	copy(cpy.Signers, s.Signers)
	for voter, vote := range s.Votes {
		cpy.Votes[voter] = &Vote{
			Voter:     vote.Voter,
			Candidate: vote.Candidate,
			Stake:     new(big.Int).Set(vote.Stake),
		}
	}
	for candidate, tally := range s.Tally {
		cpy.Tally[candidate] = tally
	}
	for voter, number := range s.Voters {
		cpy.Voters[voter] = number
	}
	for signer, cnt := range s.Punished{
		cpy.Punished[signer] = cnt
	}
	return cpy
}

// apply creates a new authorization snapshot by applying the given headers to
// the original one.
func (s *Snapshot) apply(headers []*types.Header) (*Snapshot, error) {
	// Allow passing in no headers for cleaner code
	if len(headers) == 0 {
		return s, nil
	}
	// Sanity check that the headers can be applied
	for i := 0; i < len(headers)-1; i++ {
		if headers[i+1].Number.Uint64() != headers[i].Number.Uint64()+1 {
			return nil, errInvalidVotingChain
		}
	}
	if headers[0].Number.Uint64() != s.Number+1 {
		return nil, errInvalidVotingChain
	}
	// Iterate through the headers and create a new snapshot
	snap := s.copy()

	for _, header := range headers {
		// Resolve the authorization key and check against signers
		_, err := ecrecover(header, s.sigcache)
		if err != nil {
			return nil, err
		}

		headerExtra := HeaderExtra{}
		rlp.DecodeBytes(header.Extra[extraVanity:len(header.Extra)-extraSeal], &headerExtra)
		snap.HeaderTime = header.Time.Uint64()
		snap.LoopStartTime = headerExtra.LoopStartTime
		snap.Signers = nil
		for i := range headerExtra.SignerQueue {
			snap.Signers = append(snap.Signers, &headerExtra.SignerQueue[i])
		}
		// deal the new vote from voter
		for _, vote := range headerExtra.CurrentBlockVotes {
			// update Votes, Tally, Voters data
			if lastVote, ok := snap.Votes[vote.Voter]; ok {
				snap.Tally[lastVote.Candidate].Sub(snap.Tally[lastVote.Candidate], lastVote.Stake)
			}
			if _, ok := snap.Tally[vote.Candidate]; ok {

				snap.Tally[vote.Candidate].Add(snap.Tally[vote.Candidate], vote.Stake)
			} else {
				snap.Tally[vote.Candidate] = vote.Stake
			}

			snap.Votes[vote.Voter] = &Vote{vote.Voter, vote.Candidate, vote.Stake}
			snap.Voters[vote.Voter] = header.Number
		}
		// deal the voter which balance modified
		for _, txVote := range headerExtra.ModifyPredecessorVotes {

			if lastVote, ok := snap.Votes[txVote.Voter]; ok {
				snap.Tally[lastVote.Candidate].Sub(snap.Tally[lastVote.Candidate], lastVote.Stake)
				snap.Tally[lastVote.Candidate].Add(snap.Tally[lastVote.Candidate], txVote.Stake)
				snap.Votes[txVote.Voter] = &Vote{Voter: txVote.Voter, Candidate: lastVote.Candidate, Stake: txVote.Stake}
				// do not modify header number of snap.Voters
			}
		}
		// set punished count to half of origin in Epoch
		if header.Number.Uint64() % snap.config.Epoch == 0 {
			for bePublished := range snap.Punished{
				if count := snap.Punished[bePublished] / 2; count > 0{
					snap.Punished[bePublished] = count
				}else {
					delete(snap.Punished, bePublished)
				}
			}
		}
		// punish the missing signer
		for _, signerMissing := range headerExtra.SignerMissing {
			if _, ok := snap.Punished[signerMissing]; ok {
				snap.Punished[signerMissing] += missingPublishCredit
			}else{
				snap.Punished[signerMissing] = missingPublishCredit
			}
		}
		// reduce the punish of sign signer
		if _, ok := snap.Punished[header.Coinbase]; ok {
			snap.Punished[header.Coinbase] -= signRewardCredit
			if snap.Punished[header.Coinbase] <= 0 {
				delete(snap.Punished, header.Coinbase)
			}
		}
	}
	snap.Number += uint64(len(headers))
	snap.Hash = headers[len(headers)-1].Hash()

	// deal the expired vote
	for voterAddress, voteNumber := range snap.Voters {
		if len(snap.Voters) <= int(s.config.MaxSignerCount) || len(snap.Tally) <= int(s.config.MaxSignerCount) {
			break
		}
		if snap.Number-voteNumber.Uint64() > s.config.Epoch {
			// clear the vote
			if expiredVote, ok := snap.Votes[voterAddress]; ok {
				snap.Tally[expiredVote.Candidate].Sub(snap.Tally[expiredVote.Candidate], expiredVote.Stake)
				if snap.Tally[expiredVote.Candidate].Cmp(big.NewInt(0)) == 0 {
					delete(snap.Tally, expiredVote.Candidate)
				}
				delete(snap.Votes, expiredVote.Voter)
				delete(snap.Voters, expiredVote.Voter)
			}
		}
	}
	// remove 0 stake tally
	for address, tally := range snap.Tally {
		if tally.Cmp(big.NewInt(0)) <= 0 {
			delete(snap.Tally, address)
		}
	}

	return snap, nil
}

// inturn returns if a signer at a given block height is in-turn or not.
func (s *Snapshot) inturn(signer common.Address, headerTime uint64) bool {

	// if all node stop more than period of one loop
	loopIndex := int((headerTime-s.LoopStartTime)/s.config.Period) % len(s.Signers)
	if loopIndex >= len(s.Signers) {
		return false
	} else if *s.Signers[loopIndex] != signer {
		return false

	}
	return true
}

type TallyItem struct{
	addr common.Address
	stake *big.Int
}
type TallySlice []TallyItem

func (s TallySlice) Len() int           { return len(s) }
func (s TallySlice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
func (s TallySlice) Less(i, j int) bool { return s[i].stake.Cmp(s[j].stake) > 0 }

// get signer queue when one loop finished
func (s *Snapshot) getSignerQueue() []common.Address {

	var tallySlice TallySlice
	var topStakeAddress []common.Address

	for address, stake := range s.Tally {
		if _,ok := s.Punished[address]; ok{
			creditWeight := defaultFullCredit - s.Punished[address]
			if creditWeight < minCalSignerQueueCredit { creditWeight = minCalSignerQueueCredit }
			tallySlice = append(tallySlice, TallyItem{address, new(big.Int).Mul(stake, big.NewInt(int64(creditWeight)))})
		}else{
			tallySlice = append(tallySlice, TallyItem{address, new(big.Int).Mul(stake, big.NewInt(defaultFullCredit))})
		}
	}

	sort.Sort(TallySlice(tallySlice))
	queueLength := int(s.config.MaxSignerCount)
	if queueLength > len(tallySlice){
		queueLength = len(tallySlice)
	}

	for _, tallyItem := range tallySlice[:queueLength] {
			topStakeAddress = append(topStakeAddress, tallyItem.addr)
	}
	// Set the top candidates in random order
	for i := 0; i < len(topStakeAddress); i++ {
		newPos := rand.Int() % len(topStakeAddress)
		topStakeAddress[i], topStakeAddress[newPos] = topStakeAddress[newPos], topStakeAddress[i]
	}
	return topStakeAddress
}

// check if address belong to voter
func (s *Snapshot) isVoter(address common.Address) bool {
	if _, ok := s.Voters[address]; ok {
		return true
	}
	return false
}
