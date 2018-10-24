// Copyright 2018 The dexon-consensus-core Authors
// This file is part of the dexon-consensus-core library.
//
// The dexon-consensus-core library is free software: you can redistribute it
// and/or modify it under the terms of the GNU Lesser General Public License as
// published by the Free Software Foundation, either version 3 of the License,
// or (at your option) any later version.
//
// The dexon-consensus-core library is distributed in the hope that it will be
// useful, but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser
// General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the dexon-consensus-core library. If not, see
// <http://www.gnu.org/licenses/>.

package core

import (
	"fmt"
	"sync"
	"time"

	"github.com/dexon-foundation/dexon-consensus-core/common"
	"github.com/dexon-foundation/dexon-consensus-core/core/crypto"
	"github.com/dexon-foundation/dexon-consensus-core/core/types"
)

// Errors for configuration chain..
var (
	ErrDKGNotRegistered = fmt.Errorf(
		"not yet registered in DKG protocol")
	ErrTSigAlreadyRunning = fmt.Errorf(
		"tsig is already running")
	ErrDKGNotReady = fmt.Errorf(
		"DKG is not ready")
)

type configurationChain struct {
	ID          types.NodeID
	recv        dkgReceiver
	gov         Governance
	dkg         *dkgProtocol
	logger      common.Logger
	dkgLock     sync.RWMutex
	dkgSigner   map[uint64]*dkgShareSecret
	gpk         map[uint64]*DKGGroupPublicKey
	dkgResult   sync.RWMutex
	tsig        map[common.Hash]*tsigProtocol
	tsigTouched map[common.Hash]struct{}
	tsigReady   *sync.Cond
	// TODO(jimmy-dexon): add timeout to pending psig.
	pendingPsig map[common.Hash][]*types.DKGPartialSignature
	prevHash    common.Hash
}

func newConfigurationChain(
	ID types.NodeID,
	recv dkgReceiver,
	gov Governance,
	logger common.Logger) *configurationChain {
	return &configurationChain{
		ID:          ID,
		recv:        recv,
		gov:         gov,
		logger:      logger,
		dkgSigner:   make(map[uint64]*dkgShareSecret),
		gpk:         make(map[uint64]*DKGGroupPublicKey),
		tsig:        make(map[common.Hash]*tsigProtocol),
		tsigTouched: make(map[common.Hash]struct{}),
		tsigReady:   sync.NewCond(&sync.Mutex{}),
		pendingPsig: make(map[common.Hash][]*types.DKGPartialSignature),
	}
}

func (cc *configurationChain) registerDKG(round uint64, threshold int) {
	cc.dkgLock.Lock()
	defer cc.dkgLock.Unlock()
	cc.dkg = newDKGProtocol(
		cc.ID,
		cc.recv,
		round,
		threshold)
}

func (cc *configurationChain) runDKG(round uint64) error {
	cc.dkgLock.Lock()
	defer cc.dkgLock.Unlock()
	if cc.dkg == nil || cc.dkg.round != round {
		return ErrDKGNotRegistered
	}
	if func() bool {
		cc.dkgResult.RLock()
		defer cc.dkgResult.RUnlock()
		_, exist := cc.gpk[round]
		return exist
	}() {
		return nil
	}

	ticker := newTicker(cc.gov, round, TickerDKG)
	cc.dkgLock.Unlock()
	<-ticker.Tick()
	cc.dkgLock.Lock()
	// Phase 2(T = 0): Exchange DKG secret key share.
	cc.logger.Debug("Calling Governance.DKGMasterPublicKeys", "round", round)
	cc.dkg.processMasterPublicKeys(cc.gov.DKGMasterPublicKeys(round))
	// Phase 3(T = 0~λ): Propose complaint.
	// Propose complaint is done in `processMasterPublicKeys`.
	cc.dkgLock.Unlock()
	<-ticker.Tick()
	cc.dkgLock.Lock()
	// Phase 4(T = λ): Propose nack complaints.
	cc.dkg.proposeNackComplaints()
	cc.dkgLock.Unlock()
	<-ticker.Tick()
	cc.dkgLock.Lock()
	// Phase 5(T = 2λ): Propose Anti nack complaint.
	cc.logger.Debug("Calling Governance.DKGComplaints", "round", round)
	cc.dkg.processNackComplaints(cc.gov.DKGComplaints(round))
	cc.dkgLock.Unlock()
	<-ticker.Tick()
	cc.dkgLock.Lock()
	// Phase 6(T = 3λ): Rebroadcast anti nack complaint.
	// Rebroadcast is done in `processPrivateShare`.
	cc.dkgLock.Unlock()
	<-ticker.Tick()
	cc.dkgLock.Lock()
	// Phase 7(T = 4λ): Enforce complaints and nack complaints.
	cc.logger.Debug("Calling Governance.DKGComplaints", "round", round)
	cc.dkg.enforceNackComplaints(cc.gov.DKGComplaints(round))
	// Enforce complaint is done in `processPrivateShare`.
	// Phase 8(T = 5λ): DKG finalize.
	cc.dkgLock.Unlock()
	<-ticker.Tick()
	cc.dkgLock.Lock()
	cc.dkg.proposeFinalize()
	// Phase 9(T = 6λ): DKG is ready.
	cc.dkgLock.Unlock()
	<-ticker.Tick()
	cc.dkgLock.Lock()
	// Normally, IsDKGFinal would return true here. Use this for in case of
	// unexpected network fluctuation and ensure the robustness of DKG protocol.
	cc.logger.Debug("Calling Governance.IsDKGFinal", "round", round)
	for !cc.gov.IsDKGFinal(round) {
		cc.logger.Info("DKG is not ready yet. Try again later...",
			"nodeID", cc.ID)
		time.Sleep(500 * time.Millisecond)
	}
	cc.logger.Debug("Calling Governance.DKGMasterPublicKeys", "round", round)
	cc.logger.Debug("Calling Governance.DKGComplaints", "round", round)
	gpk, err := NewDKGGroupPublicKey(round,
		cc.gov.DKGMasterPublicKeys(round),
		cc.gov.DKGComplaints(round),
		cc.dkg.threshold)
	if err != nil {
		return err
	}
	signer, err := cc.dkg.recoverShareSecret(gpk.qualifyIDs)
	if err != nil {
		return err
	}
	qualifies := ""
	for nID := range gpk.qualifyNodeIDs {
		qualifies += fmt.Sprintf("%s ", nID.String()[:6])
	}
	cc.logger.Info("Qualify Nodes",
		"nodeID", cc.ID,
		"round", round,
		"count", len(gpk.qualifyIDs),
		"qualifies", qualifies)
	cc.dkgResult.Lock()
	defer cc.dkgResult.Unlock()
	cc.dkgSigner[round] = signer
	cc.gpk[round] = gpk
	return nil
}

func (cc *configurationChain) preparePartialSignature(
	round uint64, hash common.Hash) (*types.DKGPartialSignature, error) {
	signer, exist := func() (*dkgShareSecret, bool) {
		cc.dkgResult.RLock()
		defer cc.dkgResult.RUnlock()
		signer, exist := cc.dkgSigner[round]
		return signer, exist
	}()
	if !exist {
		return nil, ErrDKGNotReady
	}
	return &types.DKGPartialSignature{
		ProposerID:       cc.ID,
		Round:            round,
		Hash:             hash,
		PartialSignature: signer.sign(hash),
	}, nil
}

func (cc *configurationChain) touchTSigHash(hash common.Hash) (first bool) {
	cc.tsigReady.L.Lock()
	defer cc.tsigReady.L.Unlock()
	_, exist := cc.tsigTouched[hash]
	cc.tsigTouched[hash] = struct{}{}
	return !exist
}

func (cc *configurationChain) runTSig(
	round uint64, hash common.Hash) (
	crypto.Signature, error) {
	gpk, exist := func() (*DKGGroupPublicKey, bool) {
		cc.dkgResult.RLock()
		defer cc.dkgResult.RUnlock()
		gpk, exist := cc.gpk[round]
		return gpk, exist
	}()
	if !exist {
		return crypto.Signature{}, ErrDKGNotReady
	}
	cc.tsigReady.L.Lock()
	defer cc.tsigReady.L.Unlock()
	if _, exist := cc.tsig[hash]; exist {
		return crypto.Signature{}, ErrTSigAlreadyRunning
	}
	cc.tsig[hash] = newTSigProtocol(gpk, hash)
	pendingPsig := cc.pendingPsig[hash]
	delete(cc.pendingPsig, hash)
	go func() {
		for _, psig := range pendingPsig {
			if err := cc.processPartialSignature(psig); err != nil {
				cc.logger.Error("failed to process partial signature",
					"nodeID", cc.ID,
					"error", err)
			}
		}
	}()
	var signature crypto.Signature
	var err error
	for func() bool {
		signature, err = cc.tsig[hash].signature()
		return err == ErrNotEnoughtPartialSignatures
	}() {
		cc.tsigReady.Wait()
	}
	delete(cc.tsig, hash)
	delete(cc.tsigTouched, hash)
	if err != nil {
		return crypto.Signature{}, err
	}
	return signature, nil
}

func (cc *configurationChain) runBlockTSig(
	round uint64, hash common.Hash) (crypto.Signature, error) {
	sig, err := cc.runTSig(round, hash)
	if err != nil {
		return crypto.Signature{}, err
	}
	cc.logger.Info("Block TSIG",
		"nodeID", cc.ID,
		"round", round,
		"signature", sig)
	return sig, nil
}

func (cc *configurationChain) runCRSTSig(
	round uint64, crs common.Hash) ([]byte, error) {
	sig, err := cc.runTSig(round, crs)
	cc.logger.Info("CRS",
		"nodeID", cc.ID,
		"round", round+1,
		"signature", sig)
	return sig.Signature[:], err
}

func (cc *configurationChain) processPrivateShare(
	prvShare *types.DKGPrivateShare) error {
	cc.dkgLock.Lock()
	defer cc.dkgLock.Unlock()
	if cc.dkg == nil {
		return nil
	}
	return cc.dkg.processPrivateShare(prvShare)
}

func (cc *configurationChain) processPartialSignature(
	psig *types.DKGPartialSignature) error {
	cc.tsigReady.L.Lock()
	defer cc.tsigReady.L.Unlock()
	if _, exist := cc.tsig[psig.Hash]; !exist {
		ok, err := verifyDKGPartialSignatureSignature(psig)
		if err != nil {
			return err
		}
		if !ok {
			return ErrIncorrectPartialSignatureSignature
		}
		cc.pendingPsig[psig.Hash] = append(cc.pendingPsig[psig.Hash], psig)
		return nil
	}
	if err := cc.tsig[psig.Hash].processPartialSignature(psig); err != nil {
		return err
	}
	cc.tsigReady.Broadcast()
	return nil
}