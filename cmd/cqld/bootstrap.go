/*
 * Copyright 2018 The CovenantSQL Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	bp "github.com/CovenantSQL/CovenantSQL/blockproducer"
	"github.com/CovenantSQL/CovenantSQL/blockproducer/types"
	pt "github.com/CovenantSQL/CovenantSQL/blockproducer/types"
	"github.com/CovenantSQL/CovenantSQL/conf"
	"github.com/CovenantSQL/CovenantSQL/crypto/kms"
	"github.com/CovenantSQL/CovenantSQL/kayak"
	ka "github.com/CovenantSQL/CovenantSQL/kayak/api"
	kt "github.com/CovenantSQL/CovenantSQL/kayak/transport"
	"github.com/CovenantSQL/CovenantSQL/metric"
	"github.com/CovenantSQL/CovenantSQL/proto"
	"github.com/CovenantSQL/CovenantSQL/route"
	"github.com/CovenantSQL/CovenantSQL/rpc"
	"github.com/CovenantSQL/CovenantSQL/twopc"
	"github.com/CovenantSQL/CovenantSQL/utils/log"
	"golang.org/x/crypto/ssh/terminal"
)

const (
	//nodeDirPattern   = "./node_%v"
	//pubKeyStoreFile  = "public.keystore"
	//privateKeyFile   = "private.key"
	//dhtFileName      = "dht.db"
	kayakServiceName = "Kayak"
)

func runNode(nodeID proto.NodeID, listenAddr string) (err error) {
	rootPath := conf.GConf.WorkingRoot

	genesis := loadGenesis()

	var masterKey []byte
	if !conf.GConf.IsTestMode {
		// read master key
		fmt.Print("Type in Master key to continue: ")
		masterKey, err = terminal.ReadPassword(syscall.Stdin)
		if err != nil {
			fmt.Printf("Failed to read Master Key: %v", err)
		}
		fmt.Println("")
	}

	err = kms.InitLocalKeyPair(conf.GConf.PrivateKeyFile, masterKey)
	if err != nil {
		log.WithError(err).Error("init local key pair failed")
		return
	}

	// init nodes
	log.Info("init peers")
	_, peers, thisNode, err := initNodePeers(nodeID, conf.GConf.PubKeyStoreFile)
	if err != nil {
		log.WithError(err).Error("init nodes and peers failed")
		return
	}

	var service *kt.ETLSTransportService
	var server *rpc.Server

	// create server
	log.Info("create server")
	if service, server, err = createServer(
		conf.GConf.PrivateKeyFile, conf.GConf.PubKeyStoreFile, masterKey, listenAddr); err != nil {
		log.WithError(err).Error("create server failed")
		return
	}

	// init storage
	log.Info("init storage")
	var st *LocalStorage
	if st, err = initStorage(conf.GConf.DHTFileName); err != nil {
		log.WithError(err).Error("init storage failed")
		return
	}

	// init kayak
	log.Info("init kayak runtime")
	var kayakRuntime *kayak.Runtime
	if _, kayakRuntime, err = initKayakTwoPC(rootPath, thisNode, peers, st, service); err != nil {
		log.WithError(err).Error("init kayak runtime failed")
		return
	}

	// init kayak and consistent
	log.Info("init kayak and consistent runtime")
	kvServer := &KayakKVServer{
		Runtime:   kayakRuntime,
		KVStorage: st,
	}
	dht, err := route.NewDHTService(conf.GConf.DHTFileName, kvServer, true)
	if err != nil {
		log.WithError(err).Error("init consistent hash failed")
		return
	}

	// set consistent handler to kayak storage
	kvServer.KVStorage.consistent = dht.Consistent

	// register service rpc
	log.Info("register dht service rpc")
	err = server.RegisterService(route.DHTRPCName, dht)
	if err != nil {
		log.WithError(err).Error("register dht service failed")
		return
	}

	// init metrics
	log.Info("register metric service rpc")
	metricService := metric.NewCollectServer()
	if err = server.RegisterService(metric.MetricServiceName, metricService); err != nil {
		log.WithError(err).Error("init metric service failed")
		return
	}

	// init block producer database service
	log.Info("register block producer database service rpc")
	var dbService *bp.DBService
	if dbService, err = initDBService(kvServer, metricService); err != nil {
		log.WithError(err).Error("init block producer db service failed")
		return
	}
	if err = server.RegisterService(route.BPDBRPCName, dbService); err != nil {
		log.WithError(err).Error("init block producer db service failed")
		return
	}

	// init main chain service
	log.Info("register main chain service rpc")
	chainConfig := bp.NewConfig(
		genesis,
		conf.GConf.BP.ChainFileName,
		server,
		peers,
		nodeID,
		2*time.Second,
		100*time.Millisecond,
	)
	chain, err := bp.NewChain(chainConfig)
	if err != nil {
		log.WithError(err).Error("init chain failed")
		return
	}
	chain.Start()
	defer chain.Stop()

	log.Info(conf.StartSucceedMessage)
	//go periodicPingBlockProducer()

	// start server
	go func() {
		server.Serve()
	}()
	defer func() {
		server.Listener.Close()
		server.Stop()
	}()

	signalCh := make(chan os.Signal, 1)
	signal.Notify(
		signalCh,
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	signal.Ignore(syscall.SIGHUP, syscall.SIGTTIN, syscall.SIGTTOU)

	<-signalCh

	return
}

func createServer(privateKeyPath, pubKeyStorePath string, masterKey []byte, listenAddr string) (service *kt.ETLSTransportService, server *rpc.Server, err error) {
	os.Remove(pubKeyStorePath)

	server = rpc.NewServer()
	if err != nil {
		return
	}

	err = server.InitRPCServer(listenAddr, privateKeyPath, masterKey)
	service = ka.NewMuxService(kayakServiceName, server)

	return
}

func initKayakTwoPC(rootDir string, node *proto.Node, peers *kayak.Peers, worker twopc.Worker, service *kt.ETLSTransportService) (config kayak.Config, runtime *kayak.Runtime, err error) {
	// create kayak config
	log.Info("create twopc config")
	config = ka.NewTwoPCConfig(rootDir, service, worker)

	// create kayak runtime
	log.Info("create kayak runtime")
	runtime, err = ka.NewTwoPCKayak(peers, config)
	if err != nil {
		return
	}

	// init runtime
	log.Info("init kayak twopc runtime")
	err = runtime.Init()

	return
}

func initDBService(kvServer *KayakKVServer, metricService *metric.CollectServer) (dbService *bp.DBService, err error) {
	var serviceMap *bp.DBServiceMap
	if serviceMap, err = bp.InitServiceMap(kvServer); err != nil {
		log.WithError(err).Error("init bp database service map failed")
		return
	}

	dbService = &bp.DBService{
		AllocationRounds: bp.DefaultAllocationRounds, //
		ServiceMap:       serviceMap,
		Consistent:       kvServer.KVStorage.consistent,
		NodeMetrics:      &metricService.NodeMetric,
	}

	return
}

//FIXME(auxten): clean up ugly periodicPingBlockProducer
func periodicPingBlockProducer() {
	var localNodeID proto.NodeID
	var err error

	// get local node id
	if localNodeID, err = kms.GetLocalNodeID(); err != nil {
		return
	}

	// get local node info
	var localNodeInfo *proto.Node
	if localNodeInfo, err = kms.GetNodeInfo(localNodeID); err != nil {
		return
	}

	log.WithField("node", localNodeInfo).Debug("construct local node info")

	go func() {
		for {
			time.Sleep(time.Second)

			// send ping requests to block producer
			bpNodeIDs := route.GetBPs()

			for _, bpNodeID := range bpNodeIDs {
				rpc.PingBP(localNodeInfo, bpNodeID)
			}
		}
	}()
}

func loadGenesis() *types.Block {
	genesisInfo := conf.GConf.BP.BPGenesis
	log.WithField("config", genesisInfo).Info("load genesis config")

	genesis := &types.Block{
		SignedHeader: types.SignedHeader{
			Header: types.Header{
				Version:    genesisInfo.Version,
				Producer:   proto.AccountAddress(genesisInfo.Producer),
				MerkleRoot: genesisInfo.MerkleRoot,
				ParentHash: genesisInfo.ParentHash,
				Timestamp:  genesisInfo.Timestamp,
			},
			BlockHash: genesisInfo.BlockHash,
		},
	}

	for _, ba := range genesisInfo.BaseAccounts {
		log.WithFields(log.Fields{
			"address":             ba.Address.String(),
			"stableCoinBalance":   ba.StableCoinBalance,
			"covenantCoinBalance": ba.CovenantCoinBalance,
		}).Debug("setting one balance fixture in genesis block")
		genesis.Transactions = append(genesis.Transactions, pt.NewBaseAccount(
			&pt.Account{
				Address:             proto.AccountAddress(ba.Address),
				StableCoinBalance:   ba.StableCoinBalance,
				CovenantCoinBalance: ba.CovenantCoinBalance,
			}))
	}

	return genesis
}
