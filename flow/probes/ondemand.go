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

package probes

import (
	"os"
	"strings"

	"github.com/redhat-cip/skydive/api"
	"github.com/redhat-cip/skydive/flow"
	"github.com/redhat-cip/skydive/logging"
	"github.com/redhat-cip/skydive/topology/graph"
	"github.com/redhat-cip/skydive/topology/graph/traversal"
)

type OnDemandProbeListener struct {
	graph.DefaultGraphListener
	Graph          *graph.Graph
	Probes         *FlowProbeBundle
	CaptureHandler api.ApiHandler
	watcher        api.StoppableWatcher
	fta            *flow.TableAllocator
	activeProbes   map[graph.Identifier]*flow.Table
	host           string
}

func (o *OnDemandProbeListener) probeFromType(n *graph.Node) *FlowProbe {
	var probeName string

	switch n.Metadata()["Type"] {
	case "ovsbridge":
		probeName = "ovssflow"
	default:
		probeName = "pcap"
	}

	probe := o.Probes.GetProbe(probeName)
	if probe == nil {
		return nil
	}

	fprobe := probe.(FlowProbe)
	return &fprobe
}

func (o *OnDemandProbeListener) registerProbe(n *graph.Node, capture *api.Capture) {
	if !IsCaptureAllowed(n) {
		logging.GetLogger().Infof("Do not register flow probe, type not supported %v", n)
		return
	}

	fprobe := o.probeFromType(n)
	if fprobe == nil {
		logging.GetLogger().Errorf("Failed to register flow probe, unknown type %v", n)
		return
	}

	if _, ok := o.activeProbes[n.ID]; ok {
		logging.GetLogger().Debugf("A probe already exists for %s", n.ID)
		return
	}

	ft := o.fta.Alloc(fprobe.AsyncFlowPipeline)
	if err := fprobe.RegisterProbe(n, capture, ft); err != nil {
		logging.GetLogger().Debugf("Failed to register flow probe: %s", err.Error())
		o.fta.Release(ft)
		return
	}

	o.activeProbes[n.ID] = ft
	o.Graph.AddMetadata(n, "State.FlowCapture", "ON")
}

func (o *OnDemandProbeListener) unregisterProbe(n *graph.Node) {
	fprobe := o.probeFromType(n)
	if fprobe == nil {
		return
	}

	if err := fprobe.UnregisterProbe(n); err != nil {
		logging.GetLogger().Debugf("Failed to unregister flow probe: %s", err.Error())
	}

	o.fta.Release(o.activeProbes[n.ID])
	delete(o.activeProbes, n.ID)
	o.Graph.AddMetadata(n, "State.FlowCapture", "OFF")
}

func (o *OnDemandProbeListener) matchGremlinExpr(node *graph.Node, gremlin string) bool {
	tr := traversal.NewGremlinTraversalParser(strings.NewReader(gremlin), o.Graph)
	ts, err := tr.Parse()
	if err != nil {
		logging.GetLogger().Errorf("Gremlin expression error: %s", err.Error())
		return false
	}

	res, err := ts.Exec()
	if err != nil {
		logging.GetLogger().Errorf("Gremlin execution error: %s", err.Error())
		return false
	}

	for _, value := range res.Values() {
		n, ok := value.(*graph.Node)
		if !ok {
			logging.GetLogger().Error("Gremlin expression doesn't return node")
			return false
		}

		if node.ID == n.ID {
			return true
		}
	}

	return false
}

func (o *OnDemandProbeListener) OnNodeAdded(n *graph.Node) {

	resources := o.CaptureHandler.Index()
	for _, resource := range resources {
		capture := resource.(*api.Capture)

		if o.matchGremlinExpr(n, capture.GremlinQuery) {
			o.registerProbe(n, capture)
		}
	}
}

func (o *OnDemandProbeListener) OnNodeUpdated(n *graph.Node) {
	o.OnNodeAdded(n)
}

func (o *OnDemandProbeListener) OnEdgeAdded(e *graph.Edge) {
	parent, child := o.Graph.GetEdgeNodes(e)
	if parent == nil || child == nil {
		return
	}

	if parent.Metadata()["Type"] == "ovsbridge" {
		o.OnNodeAdded(parent)
		return
	}

	if child.Metadata()["Type"] == "ovsbridge" {
		o.OnNodeAdded(child)
		return
	}
}

func (o *OnDemandProbeListener) OnNodeDeleted(n *graph.Node) {
	o.unregisterProbe(n)
}

func (o *OnDemandProbeListener) onCaptureAdded(capture *api.Capture) {
	o.Graph.Lock()
	defer o.Graph.Unlock()

	tr := traversal.NewGremlinTraversalParser(strings.NewReader(capture.GremlinQuery), o.Graph)
	ts, err := tr.Parse()
	if err != nil {
		logging.GetLogger().Errorf("Gremlin expression error: %s", err.Error())
		return
	}

	res, err := ts.Exec()
	if err != nil {
		logging.GetLogger().Errorf("Gremlin execution error: %s", err.Error())
		return
	}

	for _, value := range res.Values() {
		switch value.(type) {
		case *graph.Node:
			o.registerProbe(value.(*graph.Node), capture)
		case []*graph.Node:
			for _, node := range value.([]*graph.Node) {
				o.registerProbe(node, capture)
			}
		default:
			logging.GetLogger().Error("Gremlin expression doesn't return node")
			return
		}
	}
}

func (o *OnDemandProbeListener) onCaptureDeleted(capture *api.Capture) {
	o.Graph.Lock()
	defer o.Graph.Unlock()

	tr := traversal.NewGremlinTraversalParser(strings.NewReader(capture.GremlinQuery), o.Graph)
	ts, err := tr.Parse()
	if err != nil {
		logging.GetLogger().Errorf("Gremlin expression error: %s", err.Error())
		return
	}

	res, err := ts.Exec()
	if err != nil {
		logging.GetLogger().Errorf("Gremlin execution error: %s", err.Error())
		return
	}

	for _, value := range res.Values() {
		switch value.(type) {
		case *graph.Node:
			o.unregisterProbe(value.(*graph.Node))
		case []*graph.Node:
			for _, node := range value.([]*graph.Node) {
				o.unregisterProbe(node)
			}
		default:
			logging.GetLogger().Error("Gremlin expression doesn't return node")
			return
		}
	}
}

func (o *OnDemandProbeListener) onApiWatcherEvent(action string, id string, resource api.ApiResource) {
	logging.GetLogger().Debugf("New watcher event %s for %s", action, id)
	capture := resource.(*api.Capture)
	switch action {
	case "init", "create", "set", "update":
		o.onCaptureAdded(capture)
	case "expire", "delete":
		o.onCaptureDeleted(capture)
	}
}

func (o *OnDemandProbeListener) Start() error {
	o.watcher = o.CaptureHandler.AsyncWatch(o.onApiWatcherEvent)

	o.Graph.AddEventListener(o)

	return nil
}

func (o *OnDemandProbeListener) Stop() {
	o.watcher.Stop()
}

func NewOnDemandProbeListener(fb *FlowProbeBundle, g *graph.Graph, ch api.ApiHandler) (*OnDemandProbeListener, error) {
	h, err := os.Hostname()
	if err != nil {
		return nil, err
	}

	return &OnDemandProbeListener{
		Graph:          g,
		Probes:         fb,
		CaptureHandler: ch,
		host:           h,
		fta:            fb.FlowTableAllocator,
		activeProbes:   make(map[graph.Identifier]*flow.Table),
	}, nil
}
