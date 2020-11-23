package secp256k1fx

import (
	"errors"

	"github.com/ava-labs/avalanchego/vms/components/verify"
)

var (
	errNilFreezeOperation = errors.New("nil FreezeOperation")
	//errFreezeUTXOIDsNotSortedUnique = errors.New("freeze UTXO IDs must be sorted and unique")
	//errFreezeNoUTXOs                = errors.New("freeze operation would not freeze any UTXOs")
)

// TODO comment
type FreezeOutput struct {
	OutputOwners `serialize:"true"`
}

// FreezeOperation freezes some or all of an asset
type FreezeOperation struct {
	Input        `serialize:"true"`
	FreezeOutput FreezeOutput `serialize:"true"`
}

// Verify ...
func (op *FreezeOperation) Verify() error {
	switch {
	case op == nil:
		return errNilFreezeOperation
	default:
		return verify.All(&op.Input, &op.FreezeOutput)
	}
}

// Outs returns the outputs generated by this operation
func (op *FreezeOperation) Outs() []verify.State {
	// TODO is this right?
	return []verify.State{&op.FreezeOutput}
}
