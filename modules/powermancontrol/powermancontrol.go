/* vboxmanage.go: mutations for VirtualBox using the vboxmanage-rest-api
 *
 * Author: R. Eli Snyder <resnyder@lanl.gov>s
 *
 * This software is open source software available under the BSD-3 license.
 * Copyright (c) 2018, Los Alamos National Security, LLC
 * See LICENSE file for details.
 */

//go:generate protoc -I ../../core/proto/include -I proto --go_out=plugins=grpc:proto proto/powermancontrol.proto

/*
 * This module will manipulate the PhysState state field.
 * It will be restricted to Platform = vbox.
 */

package powermancontrol

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"os/exec"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/hpc/kraken/core"
	cpb "github.com/hpc/kraken/core/proto"
	"github.com/hpc/kraken/lib"
	pb "github.com/hpc/kraken/modules/powermancontrol/proto"
)

const PlatformString string = "powerman"

// ppmut helps us succinctly define our mutations
type ppmut struct {
	f       cpb.Node_PhysState // from
	t       cpb.Node_PhysState // to
	timeout string             // timeout
	// everything fails to PHYS_HANG
}

// our mutation definitions
// also we discover anything we can migrate to
var muts = map[string]ppmut{
	"UKtoOFF": ppmut{
		f:       cpb.Node_PHYS_UNKNOWN,
		t:       cpb.Node_POWER_OFF,
		timeout: "10s",
	},
	"OFFtoON": ppmut{
		f:       cpb.Node_POWER_OFF,
		t:       cpb.Node_POWER_ON,
		timeout: "10s",
	},
	"ONtoOFF": ppmut{
		f:       cpb.Node_POWER_ON,
		t:       cpb.Node_POWER_OFF,
		timeout: "10s",
	},
	"HANGtoOFF": ppmut{
		f:       cpb.Node_PHYS_HANG,
		t:       cpb.Node_POWER_OFF,
		timeout: "20s", // we need a longer timeout, because we let it sit cold for a few seconds
	},
	"UKtoHANG": ppmut{ // this one should never happen; just making sure HANG gets connected in our graph
		f:       cpb.Node_PHYS_UNKNOWN,
		t:       cpb.Node_PHYS_HANG,
		timeout: "0s",
	},
}

// modify these if you want different requires for mutations
var reqs = map[string]reflect.Value{
	"/Platform": reflect.ValueOf(PlatformString),
}

// modify this if you want excludes
var excs = map[string]reflect.Value{}

////////////////////
// PMC Object /
//////////////////

// PMC provides a power on/off interface to the vboxmanage-rest-api interface
type PMC struct {
	api   lib.APIClient
	cfg   *pb.PMCConfig
	mchan <-chan lib.Event
	dchan chan<- lib.Event
}

/*
 *lib.Module
 */
var _ lib.Module = (*PMC)(nil)

// Name returns the FQDN of the module
func (p *PMC) Name() string { return "github.com/hpc/kraken/modules/powermancontrol" }

/*
 * lib.ModuleWithConfig
 */
var _ lib.ModuleWithConfig = (*PMC)(nil)

// NewConfig returns a fully initialized default config
func (p *PMC) NewConfig() proto.Message {
	r := &pb.PMCConfig{
		NodeNames: []string{},
		ServerUrl: "type.googleapis.com/proto.PowermanControl/ApiServer",
		NameUrl:   "type.googleapis.com/proto.PowermanControl/Name",
		UuidUrl:   "type.googleapis.com/proto.PowermanControl/Uuid",
	}
	return r
}

// UpdateConfig updates the running config
func (p *PMC) UpdateConfig(cfg proto.Message) (e error) {
	if pcfg, ok := cfg.(*pb.PMCConfig); ok {
		p.cfg = pcfg
	}
	return fmt.Errorf("invalid config type")
}

// ConfigURL gives the any resolver URL for the config
func (*PMC) ConfigURL() string {
	cfg := &pb.PMCConfig{}
	any, _ := ptypes.MarshalAny(cfg)
	return any.GetTypeUrl()
}

/*
 * lib.ModuleWithMutations & lib.ModuleWithDiscovery
 */
var _ lib.ModuleWithMutations = (*PMC)(nil)
var _ lib.ModuleWithDiscovery = (*PMC)(nil)

// SetMutationChan sets the current mutation channel
// this is generally done by the API
func (p *PMC) SetMutationChan(c <-chan lib.Event) { p.mchan = c }

// SetDiscoveryChan sets the current discovery channel
// this is generally done by the API
func (p *PMC) SetDiscoveryChan(c chan<- lib.Event) { p.dchan = c }

/*
 * lib.ModuleSelfService
 */
var _ lib.ModuleSelfService = (*PMC)(nil)

// Entry is the module's executable entrypoint
func (p *PMC) Entry() {
	url := lib.NodeURLJoin(p.api.Self().String(),
		lib.URLPush(lib.URLPush("/Services", "powermancontrol"), "State"))
	p.dchan <- core.NewEvent(
		lib.Event_DISCOVERY,
		url,
		&core.DiscoveryEvent{
			Module:  p.Name(),
			URL:     url,
			ValueID: "RUN",
		},
	)

	for {
		select {
		case m := <-p.mchan:
			if m.Type() != lib.Event_STATE_MUTATION {
				p.api.Log(lib.LLERROR, "got unexpected non-mutation event")
				break
			}
			go p.handleMutation(m)
			break
		}
	}
}

// Init is used to intialize an executable module prior to entrypoint
func (p *PMC) Init(api lib.APIClient) {
	p.api = api
	p.cfg = p.NewConfig().(*pb.PMCConfig)
}

// Stop should perform a graceful exit
func (p *PMC) Stop() {
	os.Exit(0)
}

////////////////////////
// Unexported methods /
//////////////////////

func (p *PMC) handleMutation(m lib.Event) {
	if m.Type() != lib.Event_STATE_MUTATION {
		p.api.Log(lib.LLINFO, "got an unexpected event type on mutation channel")
	}

	me := m.Data().(*core.MutationEvent)
	// extract the mutating node's name and server
	vs := me.NodeCfg.GetValues([]string{p.cfg.GetNameUrl(), p.cfg.GetServerUrl()})
	if len(vs) != 2 {
		p.api.Logf(lib.LLERROR, "could not get NID and/or VBM Server for node: %s", me.NodeCfg.ID().String())
		return
	}
	name := vs[p.cfg.GetNameUrl()].String()
	srv := vs[p.cfg.GetServerUrl()].String()

	// mutation switch
	switch me.Type {
	case core.MutationEvent_MUTATE:
		switch me.Mutation[1] {
		case "UKtoOFF": // this just forces discovery
			break
		case "OFFtoON":
			go p.nodeOn(srv, name, me.NodeCfg.ID())
			break
		case "ONtoOFF":
			go p.nodeOff(srv, name, me.NodeCfg.ID())
			break
		case "HANGtoOFF":
			go p.nodeOff(srv, name, me.NodeCfg.ID())
			break
		case "UKtoHANG": // we don't actually do this
			fallthrough
		default:
			p.api.Logf(lib.LLDEBUG, "unexpected event: %s", me.Mutation[1])
		}
		break
	case core.MutationEvent_INTERRUPT:
		// nothing to do
		break
	}
}

func (p *PMC) nodeDiscover(srvName, name string, id lib.NodeID) {
	nameIn := false
	for _, n := range p.cfg.NodeNames {
		if n == name {
			nameIn = true
			break
		}
	}

	if nameIn == false {
		p.api.Logf(lib.LLERROR, "cannot control power for unknown node: %s", name)
	}
	discCmd := exec.Command("powerman", "-Q", name)

	var stdout bytes.Buffer
	discCmd.Stdout = &stdout

	err := discCmd.Run()
	if err != nil {
		p.api.Logf(lib.LLDEBUG, "Error running the nodeDiscover command: %s", err)
		return
	}

	discOut := strings.Split(stdout.String(), "\n")
	if len(discOut) != 3 {
		p.api.Logf(lib.LLDEBUG, "Unexpected length for stdout in nodeDiscover: %d", len(discOut))
		return
	}

	var ps string
	if strings.Contains(discOut[0], name) {
		ps = "POWER_ON"
	} else if strings.Contains(discOut[1], name) {
		ps = "POWER_OFF"
	} else if strings.Contains(discOut[2], name) {
		ps = "PHYS_UNKNOWN"
	} else {
		p.api.Logf(lib.LLERROR, "Node not found in powerman discovery: %s", name)
	}

	url := lib.NodeURLJoin(id.String(), "/PhysState")
	v := core.NewEvent(
		lib.Event_DISCOVERY,
		url,
		&core.DiscoveryEvent{
			Module:  p.Name(),
			URL:     url,
			ValueID: ps,
		},
	)
	p.dchan <- v
}

func (p *PMC) nodeOn(srvName, name string, id lib.NodeID) {
	nameIn := false
	for _, n := range p.cfg.NodeNames {
		if n == name {
			nameIn = true
			break
		}
	}

	if nameIn == false {
		p.api.Logf(lib.LLERROR, "cannot control power for unknown node: %s", name)
	}

	onCmd := exec.Command("powerman", "-1", name)
	err := onCmd.Run()
	if err != nil {
		p.api.Logf(lib.LLERROR, "nodeOn command for node %s failed! with error:%s", name, err.Error())
		return
	}
	p.api.Logf(lib.LLDEBUG, "nodeOn command for node %s succeeded!", name)
	url := lib.NodeURLJoin(id.String(), "/PhysState")
	v := core.NewEvent(
		lib.Event_DISCOVERY,
		url,
		&core.DiscoveryEvent{
			Module:  p.Name(),
			URL:     url,
			ValueID: "POWER_ON",
		},
	)
	p.dchan <- v
}

func (p *PMC) nodeOff(srvName, name string, id lib.NodeID) {
	nameIn := false
	for _, n := range p.cfg.NodeNames {
		if n == name {
			nameIn = true
			break
		}
	}

	if nameIn == false {
		p.api.Logf(lib.LLERROR, "cannot control power for unknown node: %s", name)
	}

	onCmd := exec.Command("powerman", "-0", name)
	err := onCmd.Run()
	if err != nil {
		p.api.Logf(lib.LLERROR, "nodeOff command for node %s failed! with error:%s", name, err.Error())
		return
	}
	p.api.Logf(lib.LLDEBUG, "nodeOff command for node %s succeeded!", name)
	url := lib.NodeURLJoin(id.String(), "/PhysState")
	v := core.NewEvent(
		lib.Event_DISCOVERY,
		url,
		&core.DiscoveryEvent{
			Module:  p.Name(),
			URL:     url,
			ValueID: "POWER_OFF",
		},
	)
	p.dchan <- v
}

func (p *PMC) discoverAll() {
	p.api.Log(lib.LLDEBUG, "polling for node state")
	ns, e := p.api.QueryReadAll()
	if e != nil {
		p.api.Logf(lib.LLERROR, "polling node query failed: %v", e)
		return
	}
	idmap := make(map[string]lib.NodeID)
	bySrv := make(map[string][]string)

	// build lists
	for _, n := range ns {
		vs := n.GetValues([]string{"/Platform", p.cfg.GetNameUrl(), p.cfg.GetServerUrl()})
		if len(vs) != 3 {
			p.api.Logf(lib.LLDEBUG, "skipping node %s, doesn't have complete VBM info", n.ID().String())
			continue
		}
		if vs["/Platform"].String() != PlatformString { // Note: this may need to be more flexible in the future
			continue
		}
		name := vs[p.cfg.GetNameUrl()].String()
		srv := vs[p.cfg.GetServerUrl()].String()
		idmap[name] = n.ID()
		bySrv[srv] = append(bySrv[srv], name)
	}

	// This is not very efficient, but we assume that this module won't be used for huge amounts of vms
	for s, ns := range bySrv {
		for _, n := range ns {
			p.nodeDiscover(s, n, idmap[n])
		}
	}
}

// initialization
func init() {
	module := &PMC{}
	mutations := make(map[string]lib.StateMutation)
	discovers := make(map[string]map[string]reflect.Value)
	drstate := make(map[string]reflect.Value)

	for m := range muts {
		dur, _ := time.ParseDuration(muts[m].timeout)
		mutations[m] = core.NewStateMutation(
			map[string][2]reflect.Value{
				"/PhysState": [2]reflect.Value{
					reflect.ValueOf(muts[m].f),
					reflect.ValueOf(muts[m].t),
				},
			},
			reqs,
			excs,
			lib.StateMutationContext_CHILD,
			dur,
			[3]string{module.Name(), "/PhysState", "PHYS_HANG"},
		)
		drstate[cpb.Node_PhysState_name[int32(muts[m].t)]] = reflect.ValueOf(muts[m].t)
	}
	discovers["/PhysState"] = drstate
	discovers["/RunState"] = map[string]reflect.Value{
		"RUN_UK": reflect.ValueOf(cpb.Node_UNKNOWN),
	}
	discovers["/Services/powermancontrol/State"] = map[string]reflect.Value{
		"RUN": reflect.ValueOf(cpb.ServiceInstance_RUN)}
	si := core.NewServiceInstance("powermancontrol", module.Name(), module.Entry, nil)

	// Register it all
	core.Registry.RegisterModule(module)
	core.Registry.RegisterServiceInstance(module, map[string]lib.ServiceInstance{si.ID(): si})
	core.Registry.RegisterDiscoverable(module, discovers)
	core.Registry.RegisterMutations(module, mutations)
}
