package node

import (
	"fmt"
	"idena-go/api"
	"idena-go/blockchain"
	"idena-go/common/eventbus"
	"idena-go/config"
	"idena-go/consensus"
	"idena-go/core/appstate"
	"idena-go/core/ceremony"
	"idena-go/core/flip"
	"idena-go/core/mempool"
	"idena-go/crypto"
	"idena-go/ipfs"
	"idena-go/keystore"
	"idena-go/log"
	"idena-go/p2p"
	"idena-go/pengings"
	"idena-go/protocol"
	"idena-go/rpc"
	"idena-go/secstore"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/tendermint/tendermint/libs/db"
)

type Node struct {
	config          *config.Config
	blockchain      *blockchain.Blockchain
	appState        *appstate.AppState
	secStore        *secstore.SecStore
	pm              *protocol.ProtocolManager
	stop            chan struct{}
	proposals       *pengings.Proposals
	votes           *pengings.Votes
	consensusEngine *consensus.Engine
	txpool          *mempool.TxPool
	flipKeyPool     *mempool.KeysPool
	rpcAPIs         []rpc.API
	httpListener    net.Listener // HTTP RPC listener socket to server API requests
	httpHandler     *rpc.Server  // HTTP RPC request handler to process the API requests
	log             log.Logger
	srv             *p2p.Server
	keyStore        *keystore.KeyStore
	fp              *flip.Flipper
	ipfsProxy       ipfs.Proxy
	bus             eventbus.Bus
	ceremony        *ceremony.ValidationCeremony
	downloader      *protocol.Downloader
}

func StartDefaultNode(path string) string {
	fileHandler, _ := log.FileHandler(filepath.Join(path, "output.log"), log.TerminalFormat(false))
	log.Root().SetHandler(log.LvlFilterHandler(log.LvlInfo, log.MultiHandler(log.StreamHandler(os.Stdout, log.LogfmtFormat()), fileHandler)))

	c := config.GetDefaultConfig(
		path,
		config.DefaultPort,
		false,
		config.DefaultRpcHost,
		config.DefaultRpcPort,
		config.DefaultBootnode,
		"",
		config.DefaultIpfsPort,
		config.DefaultNoDiscovery,
		"",
		0, 0)

	n, err := NewNode(c)

	if err != nil {
		return err.Error()
	}

	n.Start()

	return "done"
}

func NewNode(config *config.Config) (*Node, error) {

	db, err := OpenDatabase(config, "idenachain", 16, 16)

	if err != nil {
		return nil, err
	}

	keyStoreDir, err := config.KeyStoreDataDir()
	if err != nil {
		return nil, err
	}

	ipfsProxy, err := ipfs.NewIpfsProxy(config.IpfsConf)
	if err != nil {
		return nil, err
	}

	bus := eventbus.New()

	keyStore := keystore.NewKeyStore(keyStoreDir, keystore.StandardScryptN, keystore.StandardScryptP)
	secStore := secstore.NewSecStore()
	appState := appstate.NewAppState(db, bus)
	votes := pengings.NewVotes(appState)

	txpool := mempool.NewTxPool(appState, bus)
	flipKeyPool := mempool.NewKeysPool(appState, bus)

	chain := blockchain.NewBlockchain(config, db, txpool, appState, ipfsProxy, secStore, bus)
	proposals := pengings.NewProposals(chain)
	flipper := flip.NewFlipper(db, ipfsProxy, flipKeyPool, txpool, secStore, appState)
	pm := protocol.NetProtocolManager(chain, proposals, votes, txpool, flipper, bus, flipKeyPool, config.P2P)
	downloader := protocol.NewDownloader(pm, chain, ipfsProxy, appState)
	consensusEngine := consensus.NewEngine(chain, pm, proposals, config.Consensus, appState, votes, txpool, secStore, downloader)
	ceremony := ceremony.NewValidationCeremony(appState, bus, flipper, pm, secStore, db, txpool, chain, downloader)

	return &Node{
		config:          config,
		blockchain:      chain,
		pm:              pm,
		proposals:       proposals,
		appState:        appState,
		consensusEngine: consensusEngine,
		txpool:          txpool,
		log:             log.New(),
		keyStore:        keyStore,
		fp:              flipper,
		ipfsProxy:       ipfsProxy,
		secStore:        secStore,
		bus:             bus,
		flipKeyPool:     flipKeyPool,
		ceremony:        ceremony,
		downloader:      downloader,
	}, nil
}

func (node *Node) Start() {

	config := node.config.P2P
	config.Protocols = []p2p.Protocol{
		{
			Name:    "AppName",
			Version: 1,
			Run:     node.pm.HandleNewPeer,
			Length:  35,
		},
	}
	//TODO: replace with secStore
	config.PrivateKey = node.config.NodeKey()
	node.srv = &p2p.Server{
		Config: *config,
	}
	node.secStore.AddKey(crypto.FromECDSA(node.config.NodeKey()))

	if err := node.blockchain.InitializeChain(); err != nil {
		node.log.Error("Cannot initialize blockchain", "error", err.Error())
		return
	}

	node.appState.Initialize(node.blockchain.Head.Height())
	node.txpool.Initialize(node.blockchain.Head)
	node.flipKeyPool.Initialize(node.blockchain.Head)
	node.fp.Initialize()
	node.ceremony.Initialize(node.blockchain.GetBlock(node.blockchain.Head.Hash()))
	node.blockchain.ProvideApplyNewEpochFunc(node.ceremony.ApplyNewEpoch)

	node.consensusEngine.Start()
	node.srv.Start()
	node.pm.Start()

	// Configure RPC
	if err := node.startRPC(); err != nil {
		node.log.Error("Cannot start RPC endpoint", "error", err.Error())
	}
}

func (node *Node) WaitForStop() {
	<-node.stop
	node.secStore.Destroy()
}

// startRPC is a helper method to start all the various RPC endpoint during node
// startup. It's not meant to be called at any time afterwards as it makes certain
// assumptions about the state of the node.
func (node *Node) startRPC() error {
	// Gather all the possible APIs to surface
	apis := node.apis()

	if err := node.startHTTP(node.config.RPC.HTTPEndpoint(), apis, node.config.RPC.HTTPModules, node.config.RPC.HTTPCors, node.config.RPC.HTTPVirtualHosts, node.config.RPC.HTTPTimeouts); err != nil {
		return err
	}

	node.rpcAPIs = apis
	return nil
}

// startHTTP initializes and starts the HTTP RPC endpoint.
func (node *Node) startHTTP(endpoint string, apis []rpc.API, modules []string, cors []string, vhosts []string, timeouts rpc.HTTPTimeouts) error {
	// Short circuit if the HTTP endpoint isn't being exposed
	if endpoint == "" {
		return nil
	}
	listener, handler, err := rpc.StartHTTPEndpoint(endpoint, apis, modules, cors, vhosts, timeouts)
	if err != nil {
		return err
	}
	node.log.Info("HTTP endpoint opened", "url", fmt.Sprintf("http://%s", endpoint), "cors", strings.Join(cors, ","), "vhosts", strings.Join(vhosts, ","))

	node.httpListener = listener
	node.httpHandler = handler

	return nil
}

// stopHTTP terminates the HTTP RPC endpoint.
func (node *Node) stopHTTP() {
	if node.httpListener != nil {
		node.httpListener.Close()
		node.httpListener = nil

		node.log.Info("HTTP endpoint closed", "url", fmt.Sprintf("http://%s", node.config.RPC.HTTPEndpoint()))
	}
	if node.httpHandler != nil {
		node.httpHandler.Stop()
		node.httpHandler = nil
	}
}

func OpenDatabase(c *config.Config, name string, cache int, handles int) (db.DB, error) {
	return db.NewGoLevelDBWithOpts(name, c.DataDir, &opt.Options{
		OpenFilesCacheCapacity: handles,
		BlockCacheCapacity:     cache / 2 * opt.MiB,
		WriteBuffer:            cache / 4 * opt.MiB,
		Filter:                 filter.NewBloomFilter(10),
	})
}

// apis returns the collection of RPC descriptors this node offers.
func (node *Node) apis() []rpc.API {

	baseApi := api.NewBaseApi(node.consensusEngine, node.txpool, node.keyStore, node.secStore)

	return []rpc.API{
		{
			Namespace: "net",
			Version:   "1.0",
			Service:   api.NewNetApi(node.pm, node.srv, node.ipfsProxy),
			Public:    true,
		},
		{
			Namespace: "dna",
			Version:   "1.0",
			Service:   api.NewDnaApi(baseApi, node.blockchain),
			Public:    true,
		},
		{
			Namespace: "account",
			Version:   "1.0",
			Service:   api.NewAccountApi(baseApi),
			Public:    true,
		},
		{
			Namespace: "flip",
			Version:   "1.0",
			Service:   api.NewFlipApi(baseApi, node.fp, node.pm, node.ipfsProxy, node.ceremony),
			Public:    true,
		},
		{
			Namespace: "bcn",
			Version:   "1.0",
			Service:   api.NewBlockchainApi(baseApi, node.blockchain, node.ipfsProxy, node.txpool, node.downloader, node.pm),
			Public:    true,
		},
	}
}
