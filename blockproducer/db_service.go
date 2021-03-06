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

package blockproducer

import (
	"sort"
	"sync"
	"time"

	"github.com/CovenantSQL/CovenantSQL/consistent"
	"github.com/CovenantSQL/CovenantSQL/crypto/asymmetric"
	"github.com/CovenantSQL/CovenantSQL/crypto/hash"
	"github.com/CovenantSQL/CovenantSQL/crypto/kms"
	"github.com/CovenantSQL/CovenantSQL/kayak"
	"github.com/CovenantSQL/CovenantSQL/metric"
	"github.com/CovenantSQL/CovenantSQL/pow/cpuminer"
	"github.com/CovenantSQL/CovenantSQL/proto"
	"github.com/CovenantSQL/CovenantSQL/route"
	"github.com/CovenantSQL/CovenantSQL/rpc"
	ct "github.com/CovenantSQL/CovenantSQL/sqlchain/types"
	"github.com/CovenantSQL/CovenantSQL/utils/log"
	wt "github.com/CovenantSQL/CovenantSQL/worker/types"
	dto "github.com/prometheus/client_model/go"
)

const (
	// DefaultAllocationRounds defines max rounds to try allocate peers for database creation.
	DefaultAllocationRounds = 3
)

var (
	// MetricKeyFreeMemory enumerates possible free memory metric keys.
	MetricKeyFreeMemory = []string{
		"node_memory_free_bytes_total", // mac
		"node_memory_MemFree_bytes",    // linux
	}
)

type allocatedNode struct {
	NodeID       proto.NodeID
	MemoryMetric uint64
}

// DBService defines block producer database service rpc endpoint.
type DBService struct {
	AllocationRounds int
	ServiceMap       *DBServiceMap
	Consistent       *consistent.Consistent
	NodeMetrics      *metric.NodeMetricMap

	// include block producer nodes for database allocation, for test case injection
	includeBPNodesForAllocation bool
}

// CreateDatabase defines block producer create database logic.
func (s *DBService) CreateDatabase(req *CreateDatabaseRequest, resp *CreateDatabaseResponse) (err error) {
	// verify signature
	if err = req.Verify(); err != nil {
		return
	}

	// TODO(xq262144): verify identity
	// verify identity

	// create random DatabaseID
	var dbID proto.DatabaseID
	if dbID, err = s.generateDatabaseID(req.GetNodeID()); err != nil {
		return
	}

	// allocate nodes
	var peers *kayak.Peers
	if peers, err = s.allocateNodes(0, dbID, req.Header.ResourceMeta); err != nil {
		return
	}

	// TODO(lambda): call accounting features, top up deposit
	var genesisBlock *ct.Block
	if genesisBlock, err = s.generateGenesisBlock(dbID, req.Header.ResourceMeta); err != nil {
		return
	}

	defer func() {
		if err != nil {
			// TODO(lambda): release deposit on error
		}
	}()

	// call miner nodes to provide service
	var privateKey *asymmetric.PrivateKey
	if privateKey, err = kms.GetLocalPrivateKey(); err != nil {
		return
	}

	initSvcReq := new(wt.UpdateService)
	initSvcReq.Header.Op = wt.CreateDB
	initSvcReq.Header.Instance = wt.ServiceInstance{
		DatabaseID:   dbID,
		Peers:        peers,
		GenesisBlock: genesisBlock,
	}
	if err = initSvcReq.Sign(privateKey); err != nil {
		return
	}

	rollbackReq := new(wt.UpdateService)
	rollbackReq.Header.Op = wt.DropDB
	rollbackReq.Header.Instance = wt.ServiceInstance{
		DatabaseID: dbID,
	}
	if err = rollbackReq.Sign(privateKey); err != nil {
		return
	}

	if err = s.batchSendSvcReq(initSvcReq, rollbackReq, s.peersToNodes(peers)); err != nil {
		return
	}

	// save to meta
	instanceMeta := wt.ServiceInstance{
		DatabaseID:   dbID,
		Peers:        peers,
		ResourceMeta: req.Header.ResourceMeta,
		GenesisBlock: genesisBlock,
	}

	log.Debugf("generated instance meta: %v", instanceMeta)

	if err = s.ServiceMap.Set(instanceMeta); err != nil {
		// critical error
		// TODO(xq262144): critical error recover
		return err
	}

	// send response to client
	resp.Header.InstanceMeta = instanceMeta

	// sign the response
	err = resp.Sign(privateKey)

	return
}

// DropDatabase defines block producer drop database logic.
func (s *DBService) DropDatabase(req *DropDatabaseRequest, resp *DropDatabaseResponse) (err error) {
	// verify signature
	if err = req.Verify(); err != nil {
		return
	}

	// TODO(xq262144): verify identity
	// verify identity and database belonging

	// get database peers
	var instanceMeta wt.ServiceInstance
	if instanceMeta, err = s.ServiceMap.Get(req.Header.DatabaseID); err != nil {
		return
	}

	// call miner nodes to drop database
	dropDBSvcReq := new(wt.UpdateService)
	dropDBSvcReq.Header.Op = wt.DropDB
	dropDBSvcReq.Header.Instance = wt.ServiceInstance{
		DatabaseID: req.Header.DatabaseID,
	}
	if dropDBSvcReq.Header.Signee, err = kms.GetLocalPublicKey(); err != nil {
		return
	}
	var privateKey *asymmetric.PrivateKey
	if privateKey, err = kms.GetLocalPrivateKey(); err != nil {
		return
	}
	if dropDBSvcReq.Sign(privateKey); err != nil {
		return
	}

	if err = s.batchSendSvcReq(dropDBSvcReq, nil, s.peersToNodes(instanceMeta.Peers)); err != nil {
		return
	}

	// withdraw deposit from sqlchain
	// TODO(lambda): withdraw deposit and record drop database request

	// remove from meta
	if err = s.ServiceMap.Delete(req.Header.DatabaseID); err != nil {
		// critical error
		// TODO(xq262144): critical error recover
		return
	}

	// send response to client
	// nothing to set on response, only error flag

	return
}

// GetDatabase defines block producer get database logic.
func (s *DBService) GetDatabase(req *GetDatabaseRequest, resp *GetDatabaseResponse) (err error) {
	// verify signature
	if err = req.Verify(); err != nil {
		return
	}

	// TODO(xq262144): verify identity
	// verify identity and database belonging

	// fetch from meta
	var instanceMeta wt.ServiceInstance
	if instanceMeta, err = s.ServiceMap.Get(req.Header.DatabaseID); err != nil {
		return
	}

	// send response to client
	resp.Header.InstanceMeta = instanceMeta
	if resp.Header.Signee, err = kms.GetLocalPublicKey(); err != nil {
		return
	}

	var privateKey *asymmetric.PrivateKey
	if privateKey, err = kms.GetLocalPrivateKey(); err != nil {
		return
	}

	// sign the response
	err = resp.Sign(privateKey)

	return
}

// GetNodeDatabases defines block producer get node databases logic.
func (s *DBService) GetNodeDatabases(req *wt.InitService, resp *wt.InitServiceResponse) (err error) {
	// fetch from meta
	var instances []wt.ServiceInstance
	if instances, err = s.ServiceMap.GetDatabases(req.GetNodeID().ToNodeID()); err != nil {
		return
	}

	log.Debugf("current instance for node %v: %v", req.GetNodeID().ToNodeID(), instances)

	// send response to client
	resp.Header.Instances = instances
	var privateKey *asymmetric.PrivateKey
	if privateKey, err = kms.GetLocalPrivateKey(); err != nil {
		return
	}
	err = resp.Sign(privateKey)

	return
}

func (s *DBService) generateDatabaseID(reqNodeID *proto.RawNodeID) (dbID proto.DatabaseID, err error) {
	var startNonce cpuminer.Uint256

	for {
		nonceCh := make(chan cpuminer.NonceInfo)
		quitCh := make(chan struct{})
		miner := cpuminer.NewCPUMiner(quitCh)
		go miner.ComputeBlockNonce(cpuminer.MiningBlock{
			Data:      reqNodeID.CloneBytes(),
			NonceChan: nonceCh,
			Stop:      nil,
		}, startNonce, 4)

		nonce := <-nonceCh
		close(quitCh)
		close(nonceCh)

		// set start nonceCh
		startNonce = nonce.Nonce
		startNonce.Inc()
		dbID = proto.DatabaseID(nonce.Hash.String())

		log.Debugf("try generated database id %v", dbID)

		// check existence
		if _, err = s.ServiceMap.Get(dbID); err == ErrNoSuchDatabase {
			err = nil
			return
		}
	}
}

func (s *DBService) allocateNodes(lastTerm uint64, dbID proto.DatabaseID, resourceMeta wt.ResourceMeta) (peers *kayak.Peers, err error) {
	curRange := int(resourceMeta.Node)
	excludeNodes := make(map[proto.NodeID]bool)
	var allocated []allocatedNode

	if resourceMeta.Node <= 0 {
		err = ErrDatabaseAllocation
		return
	}

	if !s.includeBPNodesForAllocation {
		// add block producer nodes to exclude node list
		for _, nodeID := range route.GetBPs() {
			excludeNodes[nodeID] = true
		}
	}

	for i := 0; i != s.AllocationRounds; i++ {
		log.Debugf("node allocation round %d", i+1)

		var nodes []proto.Node

		// clear previous allocated
		allocated = allocated[:0]
		rolesFilter := []proto.ServerRole{
			proto.Miner,
		}

		if s.includeBPNodesForAllocation {
			rolesFilter = append(rolesFilter, proto.Leader, proto.Follower)
		}

		nodes, err = s.Consistent.GetNeighborsEx(string(dbID), curRange, proto.ServerRoles(rolesFilter))

		log.Debugf("found %d neighbor nodes", len(nodes))

		// TODO(xq262144): brute force implementation to be optimized
		var nodeIDs []proto.NodeID

		for _, node := range nodes {
			if _, ok := excludeNodes[node.ID]; !ok {
				nodeIDs = append(nodeIDs, node.ID)
			}
		}

		log.Debugf("found %d suitable nodes: %v", len(nodeIDs), nodeIDs)

		if len(nodeIDs) < int(resourceMeta.Node) {
			continue
		}

		// check node resource status
		metrics := s.NodeMetrics.GetMetrics(nodeIDs)

		log.Debugf("get %d metric records for %d nodes", len(metrics), len(nodeIDs))

		for nodeID, nodeMetric := range metrics {
			log.Debugf("parse metric of node %v", nodeID)
			var metricValue uint64

			// get metric
			if metricValue, err = s.getMetric(nodeMetric, MetricKeyFreeMemory); err != nil {
				log.Debugf("get node %s memory metric failed", nodeID)

				// add to excludes
				excludeNodes[nodeID] = true
				continue
			}

			// TODO(xq262144): left reserved resources check is required
			// TODO(xq262144): filesystem check to be implemented

			if resourceMeta.Memory < metricValue {
				// can allocate
				allocated = append(allocated, allocatedNode{
					NodeID:       nodeID,
					MemoryMetric: metricValue,
				})
			} else {
				log.Debugf("node %s memory metric does not meet requirements", nodeID)
				excludeNodes[nodeID] = true
			}
		}

		if len(allocated) >= int(resourceMeta.Node) {
			// sort allocated node by metric
			sort.Slice(allocated, func(i, j int) bool {
				return allocated[i].MemoryMetric > allocated[j].MemoryMetric
			})

			allocated = allocated[:int(resourceMeta.Node)]

			// build plain allocated slice
			nodeAllocated := make([]proto.NodeID, 0, len(allocated))

			for _, node := range allocated {
				nodeAllocated = append(nodeAllocated, node.NodeID)
			}

			// build peers
			return s.buildPeers(lastTerm+1, nodes, nodeAllocated)
		}

		curRange += int(resourceMeta.Node)
	}

	// allocation failed
	err = ErrDatabaseAllocation
	return
}

func (s *DBService) getMetric(metric metric.MetricMap, keys []string) (value uint64, err error) {
	for _, key := range keys {
		var rawMetric *dto.MetricFamily
		var ok bool

		if rawMetric, ok = metric[key]; !ok || rawMetric == nil {
			continue
		}

		switch rawMetric.GetType() {
		case dto.MetricType_GAUGE:
			value = uint64(rawMetric.GetMetric()[0].GetGauge().GetValue())
			return
		case dto.MetricType_COUNTER:
			value = uint64(rawMetric.GetMetric()[0].GetCounter().GetValue())
			return
		}
	}

	err = ErrMetricNotCollected

	return
}

func (s *DBService) buildPeers(term uint64, nodes []proto.Node, allocated []proto.NodeID) (peers *kayak.Peers, err error) {
	log.Debugf("build peers for allocated nodes with term: %v, allocated nodes: %v", term, allocated)
	// get local private key
	var pubKey *asymmetric.PublicKey
	if pubKey, err = kms.GetLocalPublicKey(); err != nil {
		return
	}

	var privKey *asymmetric.PrivateKey
	if privKey, err = kms.GetLocalPrivateKey(); err != nil {
		return
	}

	// get allocated node info
	allocatedMap := make(map[proto.NodeID]bool)

	for _, nodeID := range allocated {
		allocatedMap[nodeID] = true
	}

	allocatedNodes := make([]proto.Node, 0, len(allocated))

	for _, node := range nodes {
		if allocatedMap[node.ID] {
			allocatedNodes = append(allocatedNodes, node)
		}
	}

	peers = &kayak.Peers{
		Term:    term,
		PubKey:  pubKey,
		Servers: make([]*kayak.Server, len(allocated)),
	}

	for idx, node := range allocatedNodes {
		peers.Servers[idx] = &kayak.Server{
			Role:   proto.Follower,
			ID:     node.ID,
			PubKey: node.PublicKey,
		}
	}

	// choose the first node as leader, allocateNodes sort the allocated node list by memory size
	peers.Servers[0].Role = proto.Leader
	peers.Leader = peers.Servers[0]

	// sign the peers structure
	err = peers.Sign(privKey)

	return
}

func (s *DBService) generateGenesisBlock(dbID proto.DatabaseID, resourceMeta wt.ResourceMeta) (genesisBlock *ct.Block, err error) {
	// TODO(xq262144): following is stub code, real logic should be implemented in the future
	emptyHash := hash.Hash{}

	var privKey *asymmetric.PrivateKey
	if privKey, err = kms.GetLocalPrivateKey(); err != nil {
		return
	}
	var nodeID proto.NodeID
	if nodeID, err = kms.GetLocalNodeID(); err != nil {
		return
	}

	genesisBlock = &ct.Block{
		SignedHeader: ct.SignedHeader{
			Header: ct.Header{
				Version:     0x01000000,
				Producer:    nodeID,
				GenesisHash: emptyHash,
				ParentHash:  emptyHash,
				Timestamp:   time.Now().UTC(),
			},
		},
	}
	err = genesisBlock.PackAndSignBlock(privKey)

	return
}

func (s *DBService) batchSendSvcReq(req *wt.UpdateService, rollbackReq *wt.UpdateService, nodes []proto.NodeID) (err error) {
	if err = s.batchSendSingleSvcReq(req, nodes); err != nil {
		s.batchSendSingleSvcReq(rollbackReq, nodes)
	}

	return
}

func (s *DBService) batchSendSingleSvcReq(req *wt.UpdateService, nodes []proto.NodeID) (err error) {
	var wg sync.WaitGroup
	errCh := make(chan error, len(nodes))

	for _, node := range nodes {
		wg.Add(1)
		go func(s proto.NodeID, ec chan error) {
			defer wg.Done()
			var resp wt.UpdateServiceResponse
			ec <- rpc.NewCaller().CallNode(s, route.DBSDeploy.String(), req, &resp)
		}(node, errCh)
	}

	wg.Wait()
	close(errCh)
	err = <-errCh

	return
}

func (s *DBService) peersToNodes(peers *kayak.Peers) (nodes []proto.NodeID) {
	if peers == nil {
		return
	}

	nodes = make([]proto.NodeID, 0, len(peers.Servers))

	for _, s := range peers.Servers {
		nodes = append(nodes, s.ID)
	}

	return
}
