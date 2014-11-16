package sia

import (
	"errors"
	"math/big"

	"github.com/NebulousLabs/Andromeda/hash"
)

// currentProofIndex returns the index that should be used when building and
// verifying the storage proof for a file at the given window.
func (s *State) currentProofIndex(sp StorageProof) (proofIndex uint64) {
	contract := s.OpenContracts[sp.ContractID].FileContract

	windowIndex, err := contract.WindowIndex(s.Height())
	if err != nil {
		return
	}
	triggerBlock := windowIndex*contract.Start - 1
	triggerBlockID := s.CurrentPath[triggerBlock]

	indexSeed := hash.HashBytes(append(triggerBlockID[:], sp.ContractID[:]...))
	seedInt := new(big.Int).SetBytes(indexSeed[:])
	modSeed := seedInt.Mod(seedInt, big.NewInt(int64(contract.FileSize)))
	proofIndex = uint64(modSeed.Int64())

	return
}

// validProof returns err = nil if the storage proof provided is valid given
// the state context, otherwise returning an error to indicate what is invalid.
func (s *State) validProof(sp StorageProof) (err error) {
	openContract, exists := s.OpenContracts[sp.ContractID]
	if !exists {
		err = errors.New("unrecognized contract id in storage proof")
		return
	}

	// Check that the proof has not already been submitted.
	if openContract.WindowSatisfied {
		err = errors.New("storage proof has already been completed for this contract")
		return
	}

	// Check that the storage proof itself is valid.
	numSegments, err := hash.CalculateSegments(int64(openContract.FileContract.FileSize))
	if err != nil {
		return
	}
	if !hash.VerifyReaderProof(sp.Segment, sp.HashSet, numSegments, s.currentProofIndex(sp), openContract.FileContract.FileMerkleRoot) {
		err = errors.New("provided storage proof is invalid")
		return
	}

	return
}

// applyStorageProof takes a storage proof and adds any outputs created by it
// to the consensus state.
func (s *State) applyStorageProof(sp StorageProof) {
	// Set the payout of the output - payout cannot be greater than the
	// amount of funds remaining.
	openContract := s.OpenContracts[sp.ContractID]
	payout := openContract.FileContract.ValidProofPayout
	if openContract.FundsRemaining < openContract.FileContract.ValidProofPayout {
		payout = openContract.FundsRemaining
	}

	// Create the output and add it to the list of unspent outputs.
	output := Output{
		Value:     payout,
		SpendHash: openContract.FileContract.ValidProofAddress,
	}
	outputID, err := openContract.FileContract.StorageProofOutputID(openContract.ContractID, s.Height(), true)
	if err != nil {
		panic(err)
	}
	s.UnspentOutputs[outputID] = output

	// Mark the proof as complete for this window, and subtract from the
	// FundsRemaining.
	s.OpenContracts[sp.ContractID].WindowSatisfied = true
	s.OpenContracts[sp.ContractID].FundsRemaining -= payout
}

// validContract returns err = nil if the contract is valid in the current
// context of the state, and returns an error if something about the contract
// is invalid.
func (s *State) validContract(c FileContract) (err error) {
	if c.ContractFund < 0 {
		err = errors.New("contract must be funded.")
		return
	}
	if c.Start < s.Height() {
		err = errors.New("contract must start in the future.")
		return
	}
	if c.End <= c.Start {
		err = errors.New("contract duration must be at least one block.")
		return
	}

	return
}

// addContract takes a FileContract and its corresponding ContractID and adds
// it to the state.
func (s *State) addContract(contract FileContract, id ContractID) {
	openContract := OpenContract{
		FileContract:    contract,
		ContractID:      id,
		FundsRemaining:  contract.ContractFund,
		Failures:        0,
		WindowSatisfied: true, // The first window is free, because the start is in the future by mandate.
	}
	s.OpenContracts[id] = &openContract
}

// contractMaintenance checks the contract windows and storage proofs and to
// create outputs for missed proofs and contract terminations, and to advance
// any storage proof windows.
func (s *State) contractMaintenance() {
	// Scan all open contracts and perform any required maintenance on each.
	var contractsToDelete []ContractID
	for _, openContract := range s.OpenContracts {
		// Check for the window switching over.
		if (s.Height()-openContract.FileContract.Start)%openContract.FileContract.ChallengeFrequency == 0 && s.Height() > openContract.FileContract.Start {
			// Check for a missed proof.
			if openContract.WindowSatisfied == false {
				// Determine payout of missed proof.
				payout := openContract.FileContract.MissedProofPayout
				if openContract.FundsRemaining < openContract.FileContract.MissedProofPayout {
					payout = openContract.FundsRemaining
				}

				// Create the output for the missed proof.
				newOutputID, err := openContract.FileContract.StorageProofOutputID(openContract.ContractID, s.Height(), false)
				if err != nil {
					panic(err)
				}
				output := Output{
					Value:     payout,
					SpendHash: openContract.FileContract.MissedProofAddress,
				}
				s.UnspentOutputs[newOutputID] = output
				msp := MissedStorageProof{
					OutputID:   newOutputID,
					ContractID: openContract.ContractID,
				}
				s.currentBlockNode().MissedStorageProofs = append(s.currentBlockNode().MissedStorageProofs, msp)

				// Update the FundsRemaining
				openContract.FundsRemaining -= payout

				// Update the failures count.
				openContract.Failures += 1
			}
			openContract.WindowSatisfied = false
		}

		// Check for a terminated contract.
		if openContract.FundsRemaining == 0 || openContract.FileContract.End == s.Height() || openContract.FileContract.Tolerance == openContract.Failures {
			if openContract.FundsRemaining != 0 {
				// Create a new output that terminates the contract.
				contractStatus := openContract.Failures == openContract.FileContract.Tolerance
				outputID := openContract.FileContract.ContractTerminationOutputID(openContract.ContractID, contractStatus)
				output := Output{
					Value: openContract.FundsRemaining,
				}
				if openContract.FileContract.Tolerance == openContract.Failures {
					output.SpendHash = openContract.FileContract.MissedProofAddress
				} else {
					output.SpendHash = openContract.FileContract.ValidProofAddress
				}
				s.UnspentOutputs[outputID] = output
			}

			// Add the contract to contract terminations.
			s.currentBlockNode().ContractTerminations = append(s.currentBlockNode().ContractTerminations, openContract)

			// Mark contract for deletion (can't delete from a map while
			// iterating through it - results in undefined behavior of the
			// iterator.
			contractsToDelete = append(contractsToDelete, openContract.ContractID)
		}
	}

	// Delete all of the contracts that terminated.
	for _, contractID := range contractsToDelete {
		delete(s.OpenContracts, contractID)
	}
}

// inverseContractMaintenance does the inverse of contract maintenance, moving
// the state of contracts backwards instead forwards.
func (s *State) inverseContractMaintenance() {
	// Repen all contracts that terminated, and remove the corresponding output.
	for _, openContract := range s.currentBlockNode().ContractTerminations {
		s.OpenContracts[openContract.ContractID] = openContract
		contractStatus := openContract.Failures == openContract.FileContract.Tolerance
		delete(s.UnspentOutputs, openContract.FileContract.ContractTerminationOutputID(openContract.ContractID, contractStatus))
	}

	// Reverse all outputs created by missed storage proofs.
	for _, missedProof := range s.currentBlockNode().MissedStorageProofs {
		s.OpenContracts[missedProof.ContractID].FundsRemaining += s.UnspentOutputs[missedProof.OutputID].Value
		s.OpenContracts[missedProof.ContractID].Failures -= 1
		delete(s.UnspentOutputs, missedProof.OutputID)
	}
}
