// Copyright 2019 ChainSafe Systems (ON) Corp.
// This file is part of gossamer.
//
// The gossamer library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The gossamer library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the gossamer library. If not, see <http://www.gnu.org/licenses/>.

package babe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"sync"
	"time"

	"github.com/ChainSafe/gossamer/dot/types"
	"github.com/ChainSafe/gossamer/lib/crypto/sr25519"
	"github.com/ChainSafe/gossamer/lib/runtime"
	log "github.com/ChainSafe/log15"
)

var logger log.Logger

// Service contains the VRF keys for the validator, as well as BABE configuation data
type Service struct {
	ctx       context.Context
	cancel    context.CancelFunc
	paused    bool
	authority bool

	// Storage interfaces
	blockState       BlockState
	storageState     StorageState
	transactionState TransactionState
	epochState       EpochState
	epochLength      uint64

	// BABE authority keypair
	keypair *sr25519.Keypair // TODO: change to BABE keystore

	// Current runtime
	rt runtime.Instance

	// Epoch configuration data
	slotDuration uint64 // in milliseconds
	epochData    *epochData
	startSlot    uint64
	slotToProof  map[uint64]*VrfOutputAndProof // for slots where we are a producer, store the vrf output (bytes 0-32) + proof (bytes 32-96)
	isDisabled   bool

	// Channels for inter-process communication
	blockChan chan types.Block // send blocks to core service

	// State variables
	lock  sync.Mutex
	pause chan struct{}
}

// ServiceConfig represents a BABE configuration
type ServiceConfig struct {
	LogLvl               log.Lvl
	BlockState           BlockState
	StorageState         StorageState
	TransactionState     TransactionState
	EpochState           EpochState
	Keypair              *sr25519.Keypair
	Runtime              runtime.Instance
	AuthData             []*types.Authority
	ThresholdNumerator   uint64 // for development purposes
	ThresholdDenominator uint64 // for development purposes
	SlotDuration         uint64 // for development purposes; in milliseconds
	EpochLength          uint64 // for development purposes; in slots
	StartSlot            uint64 // slot to start at
	Authority            bool
}

// NewService returns a new Babe Service using the provided VRF keys and runtime
func NewService(cfg *ServiceConfig) (*Service, error) {
	if cfg.Keypair == nil && cfg.Authority {
		return nil, errors.New("cannot create BABE service as authority; no keypair provided")
	}

	if cfg.BlockState == nil {
		return nil, errors.New("blockState is nil")
	}

	if cfg.EpochState == nil {
		return nil, errors.New("epochState is nil")
	}

	if cfg.Runtime == nil {
		return nil, errors.New("runtime is nil")
	}

	logger = log.New("pkg", "babe")
	h := log.StreamHandler(os.Stdout, log.TerminalFormat())
	h = log.CallerFileHandler(h)
	logger.SetHandler(log.LvlFilterHandler(cfg.LogLvl, h))

	ctx, cancel := context.WithCancel(context.Background())

	babeService := &Service{
		ctx:              ctx,
		cancel:           cancel,
		blockState:       cfg.BlockState,
		storageState:     cfg.StorageState,
		epochState:       cfg.EpochState,
		epochLength:      cfg.EpochLength,
		keypair:          cfg.Keypair,
		rt:               cfg.Runtime,
		transactionState: cfg.TransactionState,
		slotToProof:      make(map[uint64]*VrfOutputAndProof),
		blockChan:        make(chan types.Block),
		startSlot:        cfg.StartSlot,
		pause:            make(chan struct{}),
		authority:        cfg.Authority,
	}

	var err error
	genCfg, err := babeService.rt.BabeConfiguration()

	if err != nil {
		return nil, err
	}

	err = babeService.setEpochData(cfg, genCfg)
	if err != nil {
		return nil, err
	}

	logger.Info("created service",
		"block producer", cfg.Authority,
		"slot duration (ms)", babeService.slotDuration,
		"epoch length (slots)", babeService.epochLength,
		"authorities", Authorities(babeService.epochData.authorities),
		"authority index", babeService.epochData.authorityIndex,
		"threshold", babeService.epochData.threshold.Bytes(),
	)
	return babeService, nil
}

func (b *Service) setEpochData(cfg *ServiceConfig, genCfg *types.BabeConfiguration) (err error) {
	b.epochData = &epochData{}

	// if slot duration is set via the config file, overwrite the runtime value
	if cfg.SlotDuration > 0 {
		b.slotDuration = cfg.SlotDuration
	} else {
		b.slotDuration = genCfg.SlotDuration
	}

	if cfg.AuthData == nil {
		b.epochData.authorities, err = types.BABEAuthorityRawToAuthority(genCfg.GenesisAuthorities)
		if err != nil {
			return err
		}
	} else {
		b.epochData.authorities = cfg.AuthData
	}

	if cfg.Authority {
		b.epochData.authorityIndex, err = b.getAuthorityIndex(b.epochData.authorities)
		if err != nil {
			return err
		}
	}

	if cfg.ThresholdDenominator == 0 {
		b.epochData.threshold, err = CalculateThreshold(genCfg.C1, genCfg.C2, len(b.epochData.authorities))
	} else {
		b.epochData.threshold, err = CalculateThreshold(cfg.ThresholdNumerator, cfg.ThresholdDenominator, len(b.epochData.authorities))
	}

	if err != nil {
		return err
	}

	if cfg.EpochLength > 0 {
		b.epochLength = cfg.EpochLength
	} else {
		b.epochLength = genCfg.EpochLength
	}

	return nil
}

// Start starts BABE block authoring
func (b *Service) Start() error {
	epoch, err := b.epochState.GetCurrentEpoch()
	if err != nil {
		logger.Error("failed to get current epoch", "error", err)
		return err
	}

	// TODO: initiateEpoch sigabrts w/ non authority node epoch > 1. fix this!!
	err = b.initiateEpoch(epoch, b.startSlot)
	if err != nil {
		logger.Error("failed to initiate epoch", "error", err)
		return err
	}

	go b.initiate()
	return nil
}

// SlotDuration returns the current service slot duration
func (b *Service) SlotDuration() uint64 {
	return b.slotDuration
}

// EpochLength returns the current service epoch duration
func (b *Service) EpochLength() uint64 {
	return b.epochLength
}

// Pause pauses the service ie. halts block production
func (b *Service) Pause() error {
	if b.paused {
		return errors.New("service already paused")
	}

	select {
	case b.pause <- struct{}{}:
		logger.Info("service paused")
	default:
	}

	b.paused = true
	return nil
}

// Resume resumes the service ie. resumes block production
func (b *Service) Resume() error {
	if !b.paused {
		return errors.New("service not paused")
	}

	go b.initiate()
	b.paused = false
	logger.Info("service resumed")
	return nil
}

// Stop stops the service. If stop is called, it cannot be resumed.
func (b *Service) Stop() error {
	b.lock.Lock()
	defer b.lock.Unlock()

	if b.ctx.Err() != nil {
		return errors.New("service already stopped")
	}

	b.cancel()
	close(b.blockChan)
	return nil
}

// SetRuntime sets the service's runtime
func (b *Service) SetRuntime(rt runtime.Instance) {
	b.rt = rt
}

// GetBlockChannel returns the channel where new blocks are passed
func (b *Service) GetBlockChannel() <-chan types.Block {
	return b.blockChan
}

// SetOnDisabled sets the block producer with the given index as disabled
// If this is our node, we stop producing blocks
func (b *Service) SetOnDisabled(authorityIndex uint32) {
	if authorityIndex == b.epochData.authorityIndex {
		b.isDisabled = true
	}
}

// Authorities returns the current BABE authorities
func (b *Service) Authorities() []*types.Authority {
	return b.epochData.authorities
}

// IsStopped returns true if the service is stopped (ie not producing blocks)
func (b *Service) IsStopped() bool {
	return b.ctx.Err() != nil
}

// IsPaused returns if the service is paused or not (ie. producing blocks)
func (b *Service) IsPaused() bool {
	return b.paused
}

func (b *Service) safeSend(msg types.Block) error {
	defer func() {
		if err := recover(); err != nil {
			logger.Error("recovered from panic", "error", err)
		}
	}()

	b.lock.Lock()
	defer b.lock.Unlock()

	if b.IsStopped() {
		return errors.New("Service has been stopped")
	}

	b.blockChan <- msg
	return nil
}

func (b *Service) getAuthorityIndex(Authorities []*types.Authority) (uint32, error) {
	if !b.authority {
		return 0, ErrNotAuthority
	}

	pub := b.keypair.Public()

	for i, auth := range Authorities {
		if bytes.Equal(pub.Encode(), auth.Key.Encode()) {
			return uint32(i), nil
		}
	}

	return 0, fmt.Errorf("key not in BABE authority data")
}

func (b *Service) getSlotDuration() time.Duration {
	return time.Duration(b.slotDuration * 1000000) // SlotDuration in ms, time.Duration in ns
}

func (b *Service) initiate() {
	if b.blockState == nil {
		logger.Error("block authoring", "error", "blockState is nil")
		return
	}

	if b.storageState == nil {
		logger.Error("block authoring", "error", "storageState is nil")
		return
	}

	slotNum := b.startSlot
	bestNum, err := b.blockState.BestBlockNumber()
	if err != nil {
		logger.Error("Failed to get best block number", "error", err)
		return
	}

	// check if we are starting at genesis, if not, need to calculate slot
	if bestNum.Cmp(big.NewInt(0)) == 1 && slotNum == 0 {
		// if we have at least slotTail blocks, we can run the slotTime algorithm
		if bestNum.Cmp(big.NewInt(int64(slotTail))) != -1 {
			slotNum, err = b.getCurrentSlot()
			if err != nil {
				logger.Error("cannot get current slot", "error", err)
				return
			}
		} else {
			logger.Warn("cannot use median algorithm, not enough blocks synced")

			slotNum, err = b.estimateCurrentSlot()
			if err != nil {
				logger.Error("cannot get current slot", "error", err)
				return
			}
		}
	}

	logger.Debug("calculated slot", "number", slotNum)
	b.invokeBlockAuthoring(slotNum)
}

func (b *Service) invokeBlockAuthoring(startSlot uint64) {
	currEpoch, err := b.epochState.GetCurrentEpoch()
	if err != nil {
		logger.Error("failed to get current epoch", "error", err)
		return
	}

	// get start slot for current epoch
	epochStart, err := b.epochState.GetStartSlotForEpoch(0)
	if err != nil {
		logger.Error("failed to get start slot for current epoch", "epoch", currEpoch, "error", err)
		return
	}

	intoEpoch := startSlot - epochStart
	logger.Info("current epoch", "epoch", currEpoch, "slots into epoch", intoEpoch)

	// starting slot for next epoch
	nextStartSlot := startSlot + b.epochLength - intoEpoch

	slotDone := make([]<-chan time.Time, b.epochLength-intoEpoch)
	for i := 0; i < int(b.epochLength-intoEpoch); i++ {
		slotDone[i] = time.After(b.getSlotDuration() * time.Duration(i))
	}

	for i := 0; i < int(b.epochLength-intoEpoch); i++ {
		select {
		case <-b.ctx.Done():
			return
		case <-b.pause:
			return
		case <-slotDone[i]:
			if !b.authority {
				continue
			}

			slotNum := startSlot + uint64(i)
			err = b.handleSlot(slotNum)
			if err == ErrNotAuthorized {
				logger.Debug("not authorized to produce a block in this slot", "slot", slotNum)
				continue
			} else if err != nil {
				logger.Warn("failed to handle slot", "slot", slotNum, "error", err)
				continue
			}
		}
	}

	// setup next epoch, re-invoke block authoring
	next, err := b.incrementEpoch()
	if err != nil {
		logger.Error("failed to increment epoch", "error", err)
		return
	}

	logger.Info("initiating epoch", "number", next, "start slot", nextStartSlot)

	err = b.initiateEpoch(next, nextStartSlot)
	if err != nil {
		logger.Error("failed to initiate epoch", "epoch", next, "error", err)
		return
	}

	b.invokeBlockAuthoring(nextStartSlot)
}

func (b *Service) handleSlot(slotNum uint64) error {
	if b.isDisabled {
		return ErrNotAuthorized
	}

	if b.slotToProof[slotNum] == nil {
		// if we don't have a proof already set, re-run lottery.
		proof, err := b.runLottery(slotNum)
		if err != nil {
			logger.Warn("failed to run lottery", "slot", slotNum)
			return errors.New("failed to run lottery")
		}

		if proof == nil {
			logger.Debug("not authorized to produce block", "slot", slotNum)
			return ErrNotAuthorized
		}

		b.slotToProof[slotNum] = proof
	}

	parentHeader, err := b.blockState.BestBlockHeader()
	if err != nil {
		logger.Error("block authoring", "error", err)
		return err
	}

	if parentHeader == nil {
		logger.Error("block authoring", "error", "parent header is nil")
		return err
	}

	// there is a chance that the best block header may change in the course of building the block,
	// so let's copy it first.
	parent := parentHeader.DeepCopy()

	currentSlot := Slot{
		start:    uint64(time.Now().Unix()),
		duration: b.slotDuration,
		number:   slotNum,
	}

	logger.Debug("going to build block", "parent", parent)

	// set runtime trie before building block
	// if block building is successful, store the resulting trie in the storage state
	ts, err := b.storageState.TrieState(&parent.StateRoot)
	if err != nil || ts == nil {
		logger.Error("failed to get parent trie", "parent state root", parent.StateRoot, "error", err)
		return err
	}

	tsCopy, err := ts.Copy()
	if err != nil {
		logger.Error("failed to copy parent storage trie", "error", err)
	}

	b.rt.SetContext(tsCopy)

	block, err := b.buildBlock(parent, currentSlot)
	if err != nil {
		logger.Error("block authoring", "error", err)
		return nil
	}

	// block built successfully, store resulting trie in storage state
	// TODO: why does StateRoot not match the root of the trie after building a block?
	err = b.storageState.StoreTrie(block.Header.StateRoot, tsCopy)
	if err != nil {
		logger.Error("failed to store trie in storage state", "error", err)
	}

	hash := block.Header.Hash()
	logger.Info("built block", "hash", hash.String(), "number", block.Header.Number, "slot", slotNum)
	logger.Debug("built block", "header", block.Header, "body", block.Body, "parent", parent.Hash())

	err = b.safeSend(*block)
	if err != nil {
		logger.Error("failed to send block to core", "error", err)
		return err
	}
	return nil
}

func (b *Service) vrfSign(input []byte) (out []byte, proof []byte, err error) {
	return b.keypair.VrfSign(input)
}

// CalculateThreshold calculates the slot lottery threshold
// equation: threshold = 2^128 * (1 - (1-c)^(1/len(authorities))
func CalculateThreshold(C1, C2 uint64, numAuths int) (*big.Int, error) {
	c := float64(C1) / float64(C2)
	if c > 1 {
		return nil, errors.New("invalid C1/C2: greater than 1")
	}

	// 1 / len(authorities)
	theta := float64(1) / float64(numAuths)

	// (1-c)^(theta)
	pp := 1 - c
	pp_exp := math.Pow(pp, theta)

	// 1 - (1-c)^(theta)
	p := 1 - pp_exp
	p_rat := new(big.Rat).SetFloat64(p)

	// 1 << 256
	q := new(big.Int).Lsh(big.NewInt(1), 256)

	// (1 << 128) * (1 - (1-c)^(w_k/sum(w_i)))
	return q.Mul(q, p_rat.Num()).Div(q, p_rat.Denom()), nil
}
