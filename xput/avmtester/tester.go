// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package avmtester

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	stdmath "math"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/consensus/snowstorm"
	"github.com/ava-labs/avalanchego/snow/engine/avalanche"
	"github.com/ava-labs/avalanchego/utils/codec"
	"github.com/ava-labs/avalanchego/utils/crypto"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/utils/math"
	"github.com/ava-labs/avalanchego/utils/timer"
	"github.com/ava-labs/avalanchego/utils/wrappers"
	"github.com/ava-labs/avalanchego/vms/avm"
	"github.com/ava-labs/avalanchego/vms/components/avax"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"github.com/ava-labs/avalanchego/xput"
)

const (
	defaultMinOutstandingVtxs = 50
	defaultNumAddrs           = 1000
)

var (
	errAmtZero = errors.New("amount must not positive")
)

// TestConfig is the configuration for a throughput test on the X-Chain
type TestConfig struct {
	// NumTxs to send during the test
	NumTxs int

	// The UTXO spent to pay for this test
	avax.UTXOID
	UTXOAmount uint64 `json:"amount"`

	// Controls the UTXO
	Key *crypto.PrivateKeySECP256K1R

	// Frequency of update logs
	LogFreq int

	// Txs per vertex
	BatchSize int
}

// Config for an avmtester.Tester
type Config struct {
	// The consensus engine
	Engine            *avalanche.Transitive
	NetworkID         uint32
	ChainID           ids.ID
	Clock             timer.Clock
	codec             codec.Codec
	Log               logging.Logger
	TxFee             uint64
	AvaxAssetID       ids.ID
	MinProcessingVtxs int
}

// tester is a holder for keys and UTXOs for the Avalanche DAG.
// tester implementes Tester
type tester struct {
	Config
	lock     *sync.Mutex
	keychain *secp256k1fx.Keychain // Mapping from public address to the SigningKeys
	addrs    []ids.ShortID         // List of addresses this tester controls
	utxoSet  *UTXOSet              // Mapping from utxoIDs to UTXOs
	// Asset ID --> Balance of this asset held by this wallet
	balance map[[32]byte]uint64
	// Txs that will be issued as part of this test
	txs []*avm.Tx

	// Signalled when there are fewer than the minimum number of processing vertices
	// Its lock is the engine's lock
	processingVtxsCond *sync.Cond

	// Should only be accessed when processingVtxsCond.L is held
	processingVtxs int
}

// NewTester returns a new Tester
func NewTester(config Config) (xput.Tester, error) {
	c := codec.NewDefault()
	errs := wrappers.Errs{}
	errs.Add(
		c.RegisterType(&avm.BaseTx{}),
		c.RegisterType(&avm.CreateAssetTx{}),
		c.RegisterType(&avm.OperationTx{}),
		c.RegisterType(&avm.ImportTx{}),
		c.RegisterType(&avm.ExportTx{}),
		c.RegisterType(&secp256k1fx.TransferInput{}),
		c.RegisterType(&secp256k1fx.MintOutput{}),
		c.RegisterType(&secp256k1fx.TransferOutput{}),
		c.RegisterType(&secp256k1fx.MintOperation{}),
		c.RegisterType(&secp256k1fx.Credential{}),
	)
	config.codec = c
	t := &tester{
		Config:   config,
		keychain: secp256k1fx.NewKeychain(),
		utxoSet:  &UTXOSet{},
		balance:  make(map[[32]byte]uint64),
	}
	t.processingVtxsCond = sync.NewCond(&t.Config.Engine.Ctx.Lock)
	return t, errs.Err
}

// Run the test. Assumes Init has been called.
// Returns after all the txs have been issued.
func (t *tester) Run(configIntf interface{}) (interface{}, error) {
	config, ok := configIntf.(TestConfig)
	if !ok {
		return nil, fmt.Errorf("expected TestConfig but got %T", configIntf)
	}
	t.importKey(config.Key)
	t.addUTXO(&avax.UTXO{
		UTXOID: avax.UTXOID{
			TxID:        config.TxID,
			OutputIndex: config.OutputIndex,
		},
		Asset: avax.Asset{ID: t.AvaxAssetID},
		Out: &secp256k1fx.TransferOutput{
			Amt: config.UTXOAmount,
			OutputOwners: secp256k1fx.OutputOwners{
				Locktime:  0,
				Threshold: 1,
				Addrs:     []ids.ShortID{config.Key.PublicKey().Address()},
			},
		},
	})

	testDuration := 10 * time.Minute // TODO add this to config

	// Spawn goroutine to create tx batches
	txBatchChan := make(chan []*avm.Tx, 10*t.MinProcessingVtxs) // todo replace with constant
	stopChan := make(chan struct{})
	defer func() {
		// Signal tx generator to stop when we're done
		stopChan <- struct{}{}
	}()
	go t.generateTxs(t.AvaxAssetID, config.BatchSize, txBatchChan, stopChan)

	startTime := time.Now()
	var err error
	// Issue the txs
	for time.Now().Sub(startTime) < testDuration {
		t.processingVtxsCond.L.Lock()
		for t.processingVtxs > t.MinProcessingVtxs {
			// Wait until we process some vertices before issuing more
			t.processingVtxsCond.Wait()
		}
		txBatch := <-txBatchChan

		txs := make([]snowstorm.Tx, len(txBatch))
		for i, tx := range txBatch {
			txs[i], err = t.Engine.VM.ParseTx(tx.Bytes())
			if err != nil {
				t.processingVtxsCond.L.Unlock()
				return nil, fmt.Errorf("failed to parse tx: %s", err)
			}
		}

		if err := t.Engine.Issue(txs); err != nil {
			t.processingVtxsCond.L.Unlock()
			return nil, fmt.Errorf("failed to issue tx: %s", err)
		}
		t.processingVtxsCond.L.Unlock()

		// if i == config.NumTxs-1 || (i%config.LogFreq == 0 && i != 0) {
		// 	t.Log.Info("sent %d of %d txs", (i+1)*config.BatchSize, config.NumTxs)
		// }
	}
	return nil, nil
}

// Issue is called when the given container is issued to consensus
// Assumes t.processingVtxsCond.L is held
func (t *tester) Issue(ctx *snow.Context, containerID ids.ID, container []byte) error {
	t.processingVtxs++
	return nil
}

// Accept is called when the given container is accepted by consensus
// Assumes t.processingVtxsCond.L is held
func (t *tester) Accept(ctx *snow.Context, containerID ids.ID, container []byte) error {
	t.processingVtxs--
	if t.processingVtxs < t.MinProcessingVtxs {
		t.processingVtxsCond.Signal()
	}
	return nil
}

// Reject is called when the given container is rejected by consensus
// Assumes t.processingVtxsCond.L is held
func (t *tester) Reject(ctx *snow.Context, containerID ids.ID, container []byte) error {
	t.processingVtxs--
	if t.processingVtxs < t.MinProcessingVtxs {
		t.processingVtxsCond.Signal()
	}
	return nil
}

// getAddress returns one of the addresses this wallet manages.
// If no address exists, one will be created.
func (t *tester) getAddress() (ids.ShortID, error) {
	if t.keychain.Addrs.Len() == 0 {
		return t.createAddress()
	}
	return t.keychain.Addrs.CappedList(1)[0], nil
}

// createAddress returns a new address.
// It also saves the address and the private key that controls it
// so the address can be used later
func (t *tester) createAddress() (ids.ShortID, error) {
	privKey, err := t.keychain.New()
	return privKey.PublicKey().Address(), err
}

// importKey imports a private key into this wallet
func (t *tester) importKey(sk *crypto.PrivateKeySECP256K1R) { t.keychain.Add(sk) }

// addUTXO adds a new UTXO to this wallet if this wallet may spend it
// The UTXO's output must be an avax.TransferableOut
func (t *tester) addUTXO(utxo *avax.UTXO) {
	out, ok := utxo.Out.(avax.TransferableOut)
	if !ok {
		return
	}
	if _, _, err := t.keychain.Spend(out, stdmath.MaxUint64); err == nil {
		t.utxoSet.Put(utxo)
		t.balance[utxo.AssetID().Key()] += out.Amount()
	}
}

// removeUTXO from this wallet
func (t *tester) removeUTXO(utxoID ids.ID) {
	utxo := t.utxoSet.Get(utxoID)
	if utxo == nil {
		return
	}

	assetID := utxo.AssetID()
	assetKey := assetID.Key()
	newBalance := t.balance[assetKey] - utxo.Out.(avax.TransferableOut).Amount()
	if newBalance == 0 {
		delete(t.balance, assetKey)
	} else {
		t.balance[assetKey] = newBalance
	}

	t.utxoSet.Remove(utxoID)
}

// createTx returns a tx that sends [amount] of [assetID] to [destAddr]
// Any change is sent to an address controlled by this wallet
func (t *tester) createTx(assetID ids.ID, amount uint64, destAddr ids.ShortID, time uint64) (*avm.Tx, error) {
	if amount == 0 {
		return nil, errAmtZero
	}

	amountSpent := uint64(0)
	ins := []*avax.TransferableInput{}
	keys := [][]*crypto.PrivateKeySECP256K1R{}
	for _, utxo := range t.utxoSet.UTXOs {
		if !utxo.AssetID().Equals(assetID) {
			continue
		}
		inputIntf, signers, err := t.keychain.Spend(utxo.Out, time)
		if err != nil {
			continue
		}
		input, ok := inputIntf.(avax.TransferableIn)
		if !ok {
			continue
		}
		amountSpent, err = math.Add64(amountSpent, input.Amount())
		if err != nil {
			return nil, err
		}

		in := &avax.TransferableInput{
			UTXOID: utxo.UTXOID,
			Asset:  avax.Asset{ID: assetID},
			In:     input,
		}

		ins = append(ins, in)
		keys = append(keys, signers)

		if amountSpent >= amount+t.TxFee {
			break
		}
	}

	if amountSpent < amount+t.TxFee {
		return nil, fmt.Errorf("amount spent (%d) < amount (%d)", amountSpent, amount)
	}

	avax.SortTransferableInputsWithSigners(ins, keys)

	outs := []*avax.TransferableOutput{{
		Asset: avax.Asset{ID: assetID},
		Out: &secp256k1fx.TransferOutput{
			Amt: amount,
			OutputOwners: secp256k1fx.OutputOwners{
				Locktime:  0,
				Threshold: 1,
				Addrs:     []ids.ShortID{destAddr},
			},
		},
	}}

	if changeAmt := amountSpent - amount - t.TxFee; changeAmt > 0 {
		numAddrs := len(t.addrs)
		outs = append(outs, &avax.TransferableOutput{
			Asset: avax.Asset{ID: assetID},
			Out: &secp256k1fx.TransferOutput{
				Amt: changeAmt,
				OutputOwners: secp256k1fx.OutputOwners{
					Locktime:  0,
					Threshold: 1,
					Addrs:     []ids.ShortID{t.addrs[rand.Intn(numAddrs)]},
				},
			},
		})
	}

	avax.SortTransferableOutputs(outs, t.codec)

	tx := &avm.Tx{UnsignedTx: &avm.BaseTx{BaseTx: avax.BaseTx{
		NetworkID:    t.NetworkID,
		BlockchainID: t.ChainID,
		Outs:         outs,
		Ins:          ins,
	}}}
	return tx, tx.SignSECP256K1Fx(t.codec, keys)
}

// generateTxs continuously generates tx batches of size [batchSize] and sends them over [txBatchChan]
// Returns when an error occurs during tx creation (shouldn't happen) or when a message is received on [stopChan]
func (t *tester) generateTxs(assetID ids.ID, batchSize int, txBatchChan chan []*avm.Tx, stopChan chan struct{}) error {
	for i := 0; i < defaultNumAddrs; i++ {
		addr, err := t.createAddress()
		if err != nil {
			return err
		}
		t.addrs = append(t.addrs, addr)
	}

	now := t.Clock.Unix()
	var err error
	for {
		// Make a batch of txs and send it over the channel
		txs := make([]*avm.Tx, batchSize)
		for i := 0; i < batchSize; i++ {
			txs[i], err = t.createTx(assetID, 1, ids.GenerateTestShortID(), now)
			if err != nil {
				return err
			}
			for _, utxoID := range txs[i].InputUTXOs() {
				t.removeUTXO(utxoID.InputID())
			}
			for _, utxo := range txs[i].UTXOs() {
				t.addUTXO(utxo)
			}
		}
		select {
		case <-stopChan:
			return nil
		default:
			txBatchChan <- txs
		}
	}
}

// nextTxs returns the next [n] txs to be sent as part of xput test
// If there are less than [n] txs, returns all remaining txs
// Returns error if there are no more transactions
func (t *tester) nextTxs(n int) ([]*avm.Tx, error) {
	if len(t.txs) == 0 {
		return nil, errors.New("no remaining transactions")
	}
	if len(t.txs) < n { // There aren't [n] txs
		return t.txs, nil // Return all remaining txs
	}
	txs := t.txs[:n]
	t.txs = t.txs[n:]
	return txs, nil
}

func (t *tester) String() string {
	return fmt.Sprintf(
		"Keychain:\n"+
			"%s\n"+
			"%s",
		t.keychain.PrefixedString("    "),
		t.utxoSet.PrefixedString("    "),
	)
}
