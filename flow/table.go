/*
 * Copyright (C) 2016 Red Hat, Inc.
 *
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 *
 */

package flow

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mitchellh/mapstructure"
	"github.com/redhat-cip/skydive/common"
	"github.com/redhat-cip/skydive/logging"
)

type TableQuery struct {
	Obj interface{}
}

type TableReply struct {
	Status int
	Obj    interface{}
}

type FlowSearchQuery struct {
	NodeUUIDs []string
}

type FlowSearchReply struct {
	Flows []*Flow
}

type sortByLast []*Flow

type FlowQueryFilter struct {
	// TODO add more filter elements
	NodeUUIDs []string
}

type ExpireUpdateFunc func(f []*Flow)

type FlowHandler struct {
	callback ExpireUpdateFunc
	every    time.Duration
	duration time.Duration
}

func NewFlowHandler(callback ExpireUpdateFunc, every time.Duration, duration time.Duration) *FlowHandler {
	return &FlowHandler{
		callback: callback,
		every:    every,
		duration: duration,
	}
}

type Table struct {
	sync.RWMutex
	table         map[string]*Flow
	defaultFunc   func()
	flush         chan bool
	flushDone     chan bool
	query         chan *TableQuery
	reply         chan *TableReply
	state         int64
	lockState     sync.RWMutex
	wg            sync.WaitGroup
	updateHandler *FlowHandler
	expireHandler *FlowHandler
}

func NewTable(updateHandler *FlowHandler, expireHandler *FlowHandler) *Table {
	return &Table{
		table:         make(map[string]*Flow),
		flush:         make(chan bool),
		flushDone:     make(chan bool),
		query:         make(chan *TableQuery),
		reply:         make(chan *TableReply),
		state:         common.StoppedState,
		updateHandler: updateHandler,
		expireHandler: expireHandler,
	}
}

func NewTableFromFlows(flows []*Flow, updateHandler *FlowHandler, expireHandler *FlowHandler) *Table {
	nft := NewTable(updateHandler, expireHandler)
	nft.Update(flows)
	return nft
}

func (ft *Table) String() string {
	ft.RLock()
	defer ft.RUnlock()
	return fmt.Sprintf("%d flows", len(ft.table))
}

func (ft *Table) Update(flows []*Flow) {
	ft.Lock()
	for _, f := range flows {
		if _, ok := ft.table[f.UUID]; !ok {
			ft.table[f.UUID] = f
		} else {
			ft.table[f.UUID].Statistics = f.Statistics
		}
	}
	ft.Unlock()
}

func matchQueryFilter(f *Flow, filter *FlowQueryFilter) bool {
	for _, u := range filter.NodeUUIDs {
		if f.ProbeNodeUUID == u || f.IfSrcNodeUUID == u || f.IfDstNodeUUID == u {
			return true
		}
	}

	return false
}

func (ft *Table) GetFlows(filters ...FlowQueryFilter) []*Flow {
	ft.RLock()
	defer ft.RUnlock()

	flows := make([]*Flow, 0, len(ft.table))
	for _, f := range ft.table {
		if len(filters) == 0 || matchQueryFilter(f, &filters[0]) {
			flows = append(flows, &*f)
		}
	}
	return flows
}

func (ft *Table) GetFlow(key string) *Flow {
	ft.RLock()
	defer ft.RUnlock()
	if flow, found := ft.table[key]; found {
		return flow
	}

	return nil
}

func (ft *Table) GetOrCreateFlow(key string) (*Flow, bool) {
	ft.Lock()
	defer ft.Unlock()
	if flow, found := ft.table[key]; found {
		return flow, false
	}

	new := &Flow{}
	ft.table[key] = new

	return new, true
}

/* Return a new flow.Table that contain <last> active flows */
func (ft *Table) FilterLast(last time.Duration) []*Flow {
	var flows []*Flow
	selected := time.Now().Unix() - int64((last).Seconds())
	ft.RLock()
	defer ft.RUnlock()
	for _, f := range ft.table {
		fs := f.GetStatistics()
		if fs.Last >= selected {
			flows = append(flows, f)
		}
	}
	return flows
}

func (ft *Table) SelectLayer(endpointType FlowEndpointType, list []string) []*Flow {
	meth := make(map[string][]*Flow)
	ft.RLock()
	for _, f := range ft.table {
		layerFlow := f.GetStatistics().GetEndpointsType(endpointType)
		if layerFlow == nil || layerFlow.AB.Value == "ff:ff:ff:ff:ff:ff" || layerFlow.BA.Value == "ff:ff:ff:ff:ff:ff" {
			continue
		}
		meth[layerFlow.AB.Value] = append(meth[layerFlow.AB.Value], f)
		meth[layerFlow.BA.Value] = append(meth[layerFlow.BA.Value], f)
	}
	ft.RUnlock()

	mflows := make(map[*Flow]struct{})
	var flows []*Flow
	for _, eth := range list {
		if flist, ok := meth[eth]; ok {
			for _, f := range flist {
				if _, found := mflows[f]; !found {
					mflows[f] = struct{}{}
					flows = append(flows, f)
				}
			}
		}
	}
	return flows
}

/* Internal call only, Must be called under ft.Lock() */
func (ft *Table) expired(expireBefore int64) {
	var expiredFlows []*Flow
	flowTableSzBefore := len(ft.table)
	for _, f := range ft.table {
		fs := f.GetStatistics()
		if fs.Last < expireBefore {
			duration := time.Duration(fs.Last - fs.Start)
			logging.GetLogger().Debugf("Expire flow %s Duration %v", f.UUID, duration)
			expiredFlows = append(expiredFlows, f)
		}
	}
	/* Advise Clients */
	if ft.expireHandler != nil {
		ft.expireHandler.callback(expiredFlows)
	}
	for _, f := range expiredFlows {
		delete(ft.table, f.UUID)
	}
	flowTableSz := len(ft.table)
	logging.GetLogger().Debugf("Expire Flow : removed %v ; new size %v", flowTableSzBefore-flowTableSz, flowTableSz)
}

func (ft *Table) Updated(now time.Time) {
	timepoint := now.Unix() - int64((ft.updateHandler.duration).Seconds())
	ft.RLock()
	ft.updated(timepoint)
	ft.RUnlock()
}

/* Internal call only, Must be called under ft.RLock() */
func (ft *Table) updated(updateFrom int64) {
	var updatedFlows []*Flow
	for _, f := range ft.table {
		fs := f.GetStatistics()
		if fs.Last > updateFrom {
			updatedFlows = append(updatedFlows, f)
		}
	}
	/* Advise Clients */
	if ft.updateHandler != nil {
		ft.updateHandler.callback(updatedFlows)
	}
	logging.GetLogger().Debugf("Send updated Flow %d", len(updatedFlows))
}

func (ft *Table) expireNow() {
	const Now = int64(^uint64(0) >> 1)
	ft.Lock()
	ft.expired(Now)
	ft.Unlock()
}

func (ft *Table) Expire(now time.Time) {
	timepoint := now.Unix() - int64((ft.expireHandler.duration).Seconds())
	ft.Lock()
	ft.expired(timepoint)
	ft.Unlock()
}

func (ft *Table) RegisterDefault(fn func()) {
	ft.Lock()
	ft.defaultFunc = fn
	ft.Unlock()
}

func (ft *Table) Flush() {
	ft.flush <- true
	<-ft.flushDone
}

func (s sortByLast) Len() int {
	return len(s)
}

func (s sortByLast) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s sortByLast) Less(i, j int) bool {
	return s[i].GetStatistics().Last > s[j].GetStatistics().Last
}

func (ft *Table) onFlowSearchQueryMessage(o interface{}) (*FlowSearchReply, int) {
	var fq FlowSearchQuery
	err := mapstructure.Decode(o, &fq)
	if err != nil {
		logging.GetLogger().Errorf("Unable to decode flow search message %v", o)
		return nil, 500
	}

	flows := ft.GetFlows(FlowQueryFilter{
		NodeUUIDs: fq.NodeUUIDs,
	})

	if len(flows) == 0 {
		return &FlowSearchReply{
			Flows: flows,
		}, 404
	}

	sort.Sort(sortByLast(flows))

	return &FlowSearchReply{
		Flows: flows,
	}, 200
}

func (ft *Table) onQuery(q *TableQuery) *TableReply {
	var obj interface{}
	status := 500

	switch q.Obj.(type) {
	case *FlowSearchQuery:
		obj, status = ft.onFlowSearchQueryMessage(q.Obj)
	}

	return &TableReply{
		Status: status,
		Obj:    obj,
	}
}

func (ft *Table) Query(query *TableQuery) *TableReply {
	ft.lockState.Lock()
	defer ft.lockState.Unlock()

	if atomic.LoadInt64(&ft.state) == common.RunningState {
		ft.query <- query
		r := <-ft.reply
		return r
	}
	return nil
}

func (ft *Table) Start() {
	ft.wg.Add(1)
	defer ft.wg.Done()

	updateTicker := time.NewTicker(ft.updateHandler.every)
	defer updateTicker.Stop()

	expireTicker := time.NewTicker(ft.expireHandler.every)
	defer expireTicker.Stop()

	atomic.StoreInt64(&ft.state, common.RunningState)
	for atomic.LoadInt64(&ft.state) == common.RunningState {
		select {
		case now := <-expireTicker.C:
			ft.Expire(now)
		case now := <-updateTicker.C:
			ft.Updated(now)
		case <-ft.flush:
			ft.expireNow()
			ft.flushDone <- true
		case query, ok := <-ft.query:
			if ok {
				ft.reply <- ft.onQuery(query)
			}
		default:
			if ft.defaultFunc != nil {
				ft.defaultFunc()
			} else {
				time.Sleep(20 * time.Millisecond)
			}
		}
	}
}

func (ft *Table) Stop() {
	ft.lockState.Lock()
	if atomic.CompareAndSwapInt64(&ft.state, common.RunningState, common.StoppingState) {
		ft.wg.Wait()
	}
	ft.lockState.Unlock()

	close(ft.query)
	close(ft.reply)

	ft.expireNow()
}
