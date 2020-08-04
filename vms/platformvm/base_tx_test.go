package platformvm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ava-labs/gecko/ids"
	"github.com/ava-labs/gecko/vms/components/ava"
)

func TestBaseTxVerify(t *testing.T) {
	type test struct {
		description string
		tx          *BaseTx
		shouldErr   bool
	}

	vm := defaultVM()
	vm.Ctx.Lock.Lock()
	defer func() {
		vm.Shutdown()
		vm.Ctx.Lock.Unlock()
	}()

	nilID := ids.ID{ID: nil}
	tests := []test{
		{
			"tx is nil",
			nil,
			true,
		},
		{
			"vm is nil",
			&BaseTx{
				vm:           nil,
				id:           ids.GenerateTestID(),
				BlockchainID: vm.Ctx.ChainID,
				NetworkID:    vm.Ctx.NetworkID + 1,
			},
			true,
		},
		{
			"Blockchain ID is wrong",
			&BaseTx{
				vm:           vm,
				id:           ids.GenerateTestID(),
				BlockchainID: ids.GenerateTestID(),
				NetworkID:    vm.Ctx.NetworkID + 1,
			},
			true,
		},
		{
			"tx ID is nil",
			&BaseTx{
				vm:           vm,
				id:           nilID,
				BlockchainID: vm.Ctx.ChainID,
				NetworkID:    vm.Ctx.NetworkID + 1,
			},
			true,
		},
		{
			"network ID is wrong",
			&BaseTx{
				vm:           vm,
				id:           ids.GenerateTestID(),
				BlockchainID: vm.Ctx.ChainID,
				NetworkID:    vm.Ctx.NetworkID + 1,
			},
			true,
		},
		{
			"memo is too long",
			&BaseTx{
				vm:           vm,
				BlockchainID: ids.Empty,
				Memo:         make([]byte, maxMemoSize+1),
			},
			true,
		},
		{
			"valid",
			&BaseTx{
				vm:           vm,
				id:           ids.GenerateTestID(),
				BlockchainID: vm.Ctx.ChainID,
				NetworkID:    vm.Ctx.NetworkID,
				Memo:         make([]byte, maxMemoSize),
			},
			false,
		},
	}

	for _, test := range tests {
		if err := test.tx.Verify(); err == nil && test.shouldErr {
			t.Errorf("expected test '%s' to error but got none", test.description)
		} else if err != nil && !test.shouldErr {
			t.Errorf("test '%s' shouldn't have errored but got %s", test.description, err)
		}
	}
}

func TestBaseTxMarshalJSON(t *testing.T) {
	vm := defaultVM()
	vm.Ctx.Lock.Lock()
	defer func() {
		vm.Shutdown()
		vm.Ctx.Lock.Unlock()
	}()

	blockchainID := ids.NewID([32]byte{1})
	utxoTxID := ids.NewID([32]byte{2})
	assetID := ids.NewID([32]byte{3})
	tx := &BaseTx{
		vm:           vm,
		id:           ids.NewID([32]byte{'i', 'd'}),
		BlockchainID: blockchainID,
		NetworkID:    4,
		Ins: []*ava.TransferableInput{
			&ava.TransferableInput{
				UTXOID: ava.UTXOID{TxID: utxoTxID, OutputIndex: 5},
				Asset:  ava.Asset{ID: assetID},
				In:     MockTransferable{AmountVal: 100},
			},
		},
		Outs: []*ava.TransferableOutput{
			&ava.TransferableOutput{
				Asset: ava.Asset{ID: assetID},
				Out:   MockTransferable{AmountVal: 100},
			},
		},
		Memo: []byte{1, 2, 3},
	}
	txBytes, err := json.Marshal(tx)
	if err != nil {
		t.Fatal(err)
	}
	asString := string(txBytes)
	if !strings.Contains(asString, "\"networkID\":4") {
		t.Fatal("should have network ID")
	} else if !strings.Contains(asString, "\"blockchainID\":\"SYXsAycDPUu4z2ZksJD5fh5nTDcH3vCFHnpcVye5XuJ2jArg\"") {
		t.Fatal("should have blockchainID ID")
	} else if !strings.Contains(asString, "\"inputs\":[{\"txID\":\"t64jLxDRmxo8y48WjbRALPAZuSDZ6qPVaaeDzxHA4oSojhLt\",\"outputIndex\":5,\"assetID\":\"2KdbbWvpeAShCx5hGbtdF15FMMepq9kajsNTqVvvEbhiCRSxU\",\"input\":{\"FailVerify\":false,\"AmountVal\":100}}]") {
		t.Fatal("inputs are wrong")
	} else if !strings.Contains(asString, "\"outputs\":[{\"assetID\":\"2KdbbWvpeAShCx5hGbtdF15FMMepq9kajsNTqVvvEbhiCRSxU\",\"output\":{\"FailVerify\":false,\"AmountVal\":100}}]") {
		t.Fatal("outputs are wrong")
	}
}