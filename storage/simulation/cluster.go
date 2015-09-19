// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Bram Gruneir (bram+code@cockroachlabs.com)

package main

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"

	"github.com/cockroachdb/cockroach/base"
	"github.com/cockroachdb/cockroach/gossip"
	"github.com/cockroachdb/cockroach/proto"
	"github.com/cockroachdb/cockroach/rpc"
	"github.com/cockroachdb/cockroach/storage"
	"github.com/cockroachdb/cockroach/testutils/gossiputil"
	"github.com/cockroachdb/cockroach/util/hlc"
	"github.com/cockroachdb/cockroach/util/randutil"
	"github.com/cockroachdb/cockroach/util/stop"
)

// Cluster maintains a list of all nodes, stores and ranges as well as any
// shared resources.
type Cluster struct {
	stopper       *stop.Stopper
	clock         *hlc.Clock
	rpc           *rpc.Context
	gossip        *gossip.Gossip
	storePool     *storage.StorePool
	allocator     storage.Allocator
	storeGossiper *gossiputil.StoreGossiper
	nodes         map[proto.NodeID]*Node
	stores        map[proto.StoreID]*Store
	storeIDs      []proto.StoreID // sorted
	ranges        map[proto.RangeID]*Range
	rand          *rand.Rand
	seed          int64
	epoch         int
}

// createCluster generates a new cluster using the provided stopper and the
// number of nodes supplied. Each node will have one store to start.
func createCluster(stopper *stop.Stopper, nodeCount int) *Cluster {
	rand, seed := randutil.NewPseudoRand()
	clock := hlc.NewClock(hlc.UnixNano)
	rpcContext := rpc.NewContext(&base.Context{}, clock, stopper)
	g := gossip.New(rpcContext, gossip.TestInterval, gossip.TestBootstrap)
	storePool := storage.NewStorePool(g, storage.TestTimeUntilStoreDeadOff, stopper)
	c := &Cluster{
		stopper:       stopper,
		clock:         clock,
		rpc:           rpcContext,
		gossip:        g,
		storePool:     storePool,
		allocator:     storage.MakeAllocator(storePool, storage.RebalancingOptions{}),
		storeGossiper: gossiputil.NewStoreGossiper(g),
		nodes:         make(map[proto.NodeID]*Node),
		stores:        make(map[proto.StoreID]*Store),
		ranges:        make(map[proto.RangeID]*Range),
		rand:          rand,
		seed:          seed,
	}

	// Add the nodes.
	for i := 0; i < nodeCount; i++ {
		c.addNewNodeWithStore()
	}

	// Add a single range and add to this first node's first store.
	firstRange := c.addRange()
	firstRange.addReplica(c.stores[proto.StoreID(0)])
	return c
}

// addNewNodeWithStore adds new node with a single store.
func (c *Cluster) addNewNodeWithStore() {
	nodeID := proto.NodeID(len(c.nodes))
	c.nodes[nodeID] = newNode(nodeID, c.gossip)
	c.addStore(nodeID)
}

// addStore adds a new store to the node with the provided nodeID.
func (c *Cluster) addStore(nodeID proto.NodeID) *Store {
	n := c.nodes[nodeID]
	s := n.addNewStore()
	storeID, _ := s.getIDs()
	c.stores[storeID] = s

	// Save a sorted array of store IDs since to avoid having to calculate them
	// multiple times.
	c.storeIDs = append(c.storeIDs, storeID)
	sort.Sort(proto.StoreIDSlice(c.storeIDs))

	return s
}

// addRange adds a new range to the cluster but does not attach it to any
// store.
func (c *Cluster) addRange() *Range {
	rangeID := proto.RangeID(len(c.ranges))
	newRng := newRange(rangeID, c.allocator)
	c.ranges[rangeID] = newRng
	return newRng
}

// splitRangeRandom splits a random range from within the cluster.
func (c *Cluster) splitRangeRandom() {
	rangeID := proto.RangeID(c.rand.Int63n(int64(len(c.ranges))))
	c.splitRange(rangeID)
}

// splitRangeLast splits the last added range in the cluster.
func (c *Cluster) splitRangeLast() {
	rangeID := proto.RangeID(len(c.ranges) - 1)
	c.splitRange(rangeID)
}

// splitRange "splits" a range. This split creates a new range with new
// replicas on the same stores as the passed in range. The new range has the
// same zone config as the original range.
func (c *Cluster) splitRange(rangeID proto.RangeID) {
	newRange := c.addRange()
	originalRange := c.ranges[rangeID]
	newRange.splitRange(originalRange)
}

// runEpoch steps through a single instance of the simulator. Each epoch
// performs the following steps.
// 1) The status of every store is gossiped so the store pool is up to date.
// 2) Each replica on every range calls the allocator to determine if there are
//    any actions required.
// 3) The replica on each range with the highest priority executes it's action.
// 4) The current status of the cluster is output.
func (c *Cluster) runEpoch() {
	c.epoch++

	// Gossip all the store updates.
	c.gossipStores()

	// Determine next operation for all ranges.
	for _, r := range c.ranges {
		r.prepareActions()
	}

	// Execute the determined operations.
	c.performActions()

	// Output the update.
	fmt.Println(c.StringEpoch())
}

// gossipStores gossips all the most recent status for all stores.
func (c *Cluster) gossipStores() {
	storesRangeCounts := make(map[proto.StoreID]int)
	for _, r := range c.ranges {
		for _, storeID := range r.getStoreIDs() {
			storesRangeCounts[storeID]++
		}
	}

	c.storeGossiper.GossipWithFunction(c.storeIDs, func() {
		for storeID, store := range c.stores {
			store.gossipStore(storesRangeCounts[storeID])
		}
	})
}

// performActions performs a single action, if required, for each range.
func (c *Cluster) performActions() {
	for rangeID, r := range c.ranges {
		nextAction := r.getNextAction()
		switch nextAction.action {
		case storage.AAAdd:
			newStoreID := r.getAllocateTarget()
			r.addReplica(c.stores[newStoreID])
		case storage.AARemoveDead:
			// TODO(bram): implement this.
			fmt.Printf("Range %d - Repair\n", rangeID)
		case storage.AARemove:
			// TODO(bram): implement this.
			fmt.Printf("Range %d - Remove\n", rangeID)
		case storage.AANoop:
			if nextAction.rebalance {
				// TODO(bram): implement this.
				fmt.Printf("Range %d - Rebalance\n", rangeID)
			}
		}
	}
}

// String prints out the current status of the cluster.
func (c *Cluster) String() string {
	var buf bytes.Buffer
	buf.WriteString("Cluster Info:\n")
	buf.WriteString(fmt.Sprintf("Seed - %d:\tEpoch - %d\n", c.seed, c.epoch))
	storesRangeCounts := make(map[proto.StoreID]int)
	for _, r := range c.ranges {
		for _, storeID := range r.getStoreIDs() {
			storesRangeCounts[storeID]++
		}
	}

	var nodeIDs []proto.NodeID
	for nodeID := range c.nodes {
		nodeIDs = append(nodeIDs, nodeID)
	}
	sort.Sort(proto.NodeIDSlice(nodeIDs))

	buf.WriteString("Node Info:\n")
	for _, nodeID := range nodeIDs {
		n := c.nodes[proto.NodeID(nodeID)]
		buf.WriteString(n.String())
		buf.WriteString("\n")
	}

	buf.WriteString("Store Info:\n")
	for _, storeID := range c.storeIDs {
		s := c.stores[proto.StoreID(storeID)]
		buf.WriteString(s.String(storesRangeCounts[proto.StoreID(storeID)]))
		buf.WriteString("\n")
	}

	var rangeIDs []proto.RangeID
	for rangeID := range c.ranges {
		rangeIDs = append(rangeIDs, rangeID)
	}
	sort.Sort(proto.RangeIDSlice(rangeIDs))

	buf.WriteString("Range Info:\n")
	for _, rangeID := range rangeIDs {
		r := c.ranges[proto.RangeID(rangeID)]
		buf.WriteString(r.String())
		buf.WriteString("\n")
	}

	return buf.String()
}

// StringEpochHeader creates the string header for epoch outputs based on all
// of the current stores.
func (c *Cluster) StringEpochHeader() string {
	var buf bytes.Buffer
	buf.WriteString("Store:\t")
	for _, storeID := range c.storeIDs {
		buf.WriteString(fmt.Sprintf("%d\t", storeID))
	}
	return buf.String()
}

// StringEpoch create a string with the current free capacity for all stores.
func (c *Cluster) StringEpoch() string {
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("%d:\t", c.epoch))

	// TODO(bram): Consider saving this map in the cluster instead of
	// recalculating it each time.
	storesRangeCounts := make(map[proto.StoreID]int)
	for _, r := range c.ranges {
		for _, storeID := range r.getStoreIDs() {
			storesRangeCounts[storeID]++
		}
	}

	for _, storeID := range c.storeIDs {
		store := c.stores[proto.StoreID(storeID)]
		capacity := store.getCapacity(storesRangeCounts[proto.StoreID(storeID)])
		buf.WriteString(fmt.Sprintf("%.0f%%\t", float64(capacity.Available)/float64(capacity.Capacity)*100))
	}
	return buf.String()
}
