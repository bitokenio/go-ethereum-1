// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eth

import (
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/clique"
	"github.com/ethereum/go-ethereum/consensus/istanbul"
	istanbulBackend "github.com/ethereum/go-ethereum/consensus/istanbul/backend"
	"math/big"
	"sort"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/params"
)

var (
	// testKey is a private key to use for funding a tester account.
	testKey, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")

	// testAddr is the Ethereum address of the tester account.
	testAddr = crypto.PubkeyToAddress(testKey.PublicKey)
)

// testTxPool is a mock transaction pool that blindly accepts all transactions.
// Its goal is to get around setting up a valid statedb for the balance and nonce
// checks.
type testTxPool struct {
	pool  map[common.Hash]*types.Transaction // Hash map of collected transactions
	added chan<- []*types.Transaction        // Notification channel for new transactions

	txFeed event.Feed   // Notification feed to allow waiting for inclusion
	lock   sync.RWMutex // Protects the transaction pool
}

// newTestTxPool creates a mock transaction pool.
func newTestTxPool() *testTxPool {
	return &testTxPool{
		pool: make(map[common.Hash]*types.Transaction),
	}
}

// Has returns an indicator whether txpool has a transaction
// cached with the given hash.
func (p *testTxPool) Has(hash common.Hash) bool {
	p.lock.Lock()
	defer p.lock.Unlock()

	return p.pool[hash] != nil
}

// Get retrieves the transaction from local txpool with given
// tx hash.
func (p *testTxPool) Get(hash common.Hash) *types.Transaction {
	p.lock.Lock()
	defer p.lock.Unlock()

	return p.pool[hash]
}

// AddRemotes appends a batch of transactions to the pool, and notifies any
// listeners if the addition channel is non nil
func (p *testTxPool) AddRemotes(txs []*types.Transaction) []error {
	p.lock.Lock()
	defer p.lock.Unlock()

	for _, tx := range txs {
		p.pool[tx.Hash()] = tx
	}
	p.txFeed.Send(core.NewTxsEvent{Txs: txs})
	return make([]error, len(txs))
}

// Pending returns all the transactions known to the pool
func (p *testTxPool) Pending() (map[common.Address]types.Transactions, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	batches := make(map[common.Address]types.Transactions)
	for _, tx := range p.pool {
		from, _ := types.Sender(types.HomesteadSigner{}, tx)
		batches[from] = append(batches[from], tx)
	}
	for _, batch := range batches {
		sort.Sort(types.TxByNonce(batch))
	}
	return batches, nil
}

// SubscribeNewTxsEvent should return an event subscription of NewTxsEvent and
// send events to the given channel.
func (p *testTxPool) SubscribeNewTxsEvent(ch chan<- core.NewTxsEvent) event.Subscription {
	return p.txFeed.Subscribe(ch)
}

// testHandler is a live implementation of the Ethereum protocol handler, just
// preinitialized with some sane testing defaults and the transaction pool mocked
// out.
type testHandler struct {
	db      ethdb.Database
	chain   *core.BlockChain
	txpool  *testTxPool
	handler *handler
}

// newTestHandler creates a new handler for testing purposes with no blocks.
func newTestHandler() *testHandler {
	return newTestHandlerWithBlocks(0)
}

// newTestHandlerWithBlocks creates a new handler for testing purposes, with a
// given number of initial blocks.
func newTestHandlerWithBlocks(blocks int) *testHandler {
	// Create a database pre-initialize with a genesis block
	db := rawdb.NewMemoryDatabase()
	(&core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  core.GenesisAlloc{testAddr: {Balance: big.NewInt(1000000)}},
	}).MustCommit(db)

	chain, _ := core.NewBlockChain(db, nil, params.TestChainConfig, ethash.NewFaker(), vm.Config{}, nil, nil)

	bs, _ := core.GenerateChain(params.TestChainConfig, chain.Genesis(), ethash.NewFaker(), db, blocks, nil)
	if _, err := chain.InsertChain(bs); err != nil {
		panic(err)
	}
	txpool := newTestTxPool()

	handler, _ := newHandler(&handlerConfig{
		Database:   db,
		Chain:      chain,
		TxPool:     txpool,
		Network:    1,
		Sync:       downloader.FastSync,
		BloomCache: 1,
	})
	handler.Start(1000)

	return &testHandler{
		db:      db,
		chain:   chain,
		txpool:  txpool,
		handler: handler,
	}
}

// close tears down the handler and all its internal constructs.
func (b *testHandler) close() {
	b.handler.Stop()
	b.chain.Stop()
}

var (
	testBankKey, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	testBank       = crypto.PubkeyToAddress(testBankKey.PublicKey)
)

// Tests that correct consensus mechanism details are returned in NodeInfo.
func TestNodeInfo(t *testing.T) {

	// Define the tests to be run
	tests := []struct {
		consensus      string
		cliqueConfig   *params.CliqueConfig
		istanbulConfig *params.IstanbulConfig
		raftMode       bool
	}{
		{"ethash", nil, nil, false},
		//{"raft", nil, nil, true},
		{"istanbul", nil, &params.IstanbulConfig{1, 1, big.NewInt(0)}, false},
		//{"clique", &params.CliqueConfig{1, 1, 0}, nil, false},
	}

	// Make sure anything we screw up is restored
	backup := consensus.EthProtocol.Versions
	defer func() { consensus.EthProtocol.Versions = backup }()

	// Try all available consensus mechanisms and check for errors
	for i, tt := range tests {

		pm, _, err := newTestProtocolManagerConsensus(tt.consensus, tt.cliqueConfig, tt.istanbulConfig, tt.raftMode)

		if pm != nil {
			defer pm.Stop()
		}
		if err == nil {
			pmConsensus := getConsensusAlgorithm(pm.engine, pm.raftMode)
			if tt.consensus != pmConsensus {
				t.Errorf("test %d: consensus type error, wanted %v but got %v", i, tt.consensus, pmConsensus)
			}
		} else {
			t.Errorf("test %d: consensus type error %v", i, err)
		}
	}
}

// Quorum
func getConsensusAlgorithm(engine consensus.Engine, raftMode bool) string {
	var consensusAlgo string
	if raftMode { // raft does not use consensus interface
		consensusAlgo = "raft"
	} else {
		switch engine.(type) {
		case consensus.Istanbul:
			consensusAlgo = "istanbul"
		case *clique.Clique:
			consensusAlgo = "clique"
		case *ethash.Ethash:
			consensusAlgo = "ethash"
		default:
			consensusAlgo = "unknown"
		}
	}
	return consensusAlgo
}

// newTestProtocolManagerConsensus creates a new protocol manager for testing purposes,
// that uses the specified consensus mechanism.
func newTestProtocolManagerConsensus(consensusAlgo string, cliqueConfig *params.CliqueConfig, istanbulConfig *params.IstanbulConfig, raftMode bool) (*handler, ethdb.Database, error) {

	config := params.QuorumTestChainConfig
	config.Istanbul = istanbulConfig

	var (
		blocks                  = 0
		evmux                   = new(event.TypeMux)
		engine consensus.Engine = ethash.NewFaker()
		db                      = rawdb.NewMemoryDatabase()
		gspec                   = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc:  core.GenesisAlloc{testBank: {Balance: big.NewInt(1000000)}},
		}
		genesis       = gspec.MustCommit(db)
		blockchain, _ = core.NewBlockChain(db, nil, gspec.Config, engine, vm.Config{}, nil, nil)
	)
	chain, _ := core.GenerateChain(gspec.Config, genesis, ethash.NewFaker(), db, blocks, nil)
	if _, err := blockchain.InsertChain(chain); err != nil {
		panic(err)
	}

	switch consensusAlgo {
	case "raft":
		engine = ethash.NewFaker() //raft doesn't use engine, but just mirroring what runtime code does

	case "istanbul":
		var istCfg istanbul.Config
		config.Istanbul.Epoch = istanbulConfig.Epoch
		config.Istanbul.ProposerPolicy = istanbulConfig.ProposerPolicy

		nodeKey, _ := crypto.GenerateKey()
		engine = istanbulBackend.New(&istCfg, nodeKey, db)

	case "clique":
		engine = clique.New(config.Clique, db)

	default:
		engine = ethash.NewFaker()
	}

	pm, err := newHandler(&handlerConfig{
		Database:   db,
		Chain:      blockchain,
		TxPool:     &testTxPool{added: nil},
		Network:    DefaultConfig.NetworkId,
		Sync:       downloader.FullSync,
		BloomCache: uint64(1),
		EventMux:   evmux,
		Checkpoint: nil,
		Whitelist:  nil,
		RaftMode:   raftMode,
		Engine:     engine,
	})

	//pm, err := NewProtocolManager(config, nil, downloader.FullSync, DefaultConfig.NetworkId, evmux, &testTxPool{added: nil}, engine, blockchain, db, 1, nil, raftMode)
	if err != nil {
		return nil, nil, err
	}
	pm.Start(1000)
	return pm, db, nil
}
