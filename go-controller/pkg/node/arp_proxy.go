// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package node

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/bridgeconfig"
	nodetypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/types"
)

// arpProxyManager watches the host neighbor table via netlink and programs
// ARP responder flows on the gateway bridge (breth0) so that ARP requests
// for known neighbors are answered locally instead of being broadcast-flooded
// to all patch ports. This prevents the OVS 4096 resubmit limit from being
// exceeded when many UDNs are present.
type arpProxyManager struct {
	gwBridge      *bridgeconfig.BridgeConfiguration
	ofm           *openflowManager
	bridgeIfIndex int

	neighbors  map[string]net.HardwareAddr
	neighMutex sync.Mutex

	syncPeriod time.Duration
}

func newARPProxyManager(ofm *openflowManager, gwBridge *bridgeconfig.BridgeConfiguration) *arpProxyManager {
	return &arpProxyManager{
		gwBridge:   gwBridge,
		ofm:        ofm,
		neighbors:  make(map[string]net.HardwareAddr),
		syncPeriod: 30 * time.Second,
	}
}

func (a *arpProxyManager) Run(stopChan <-chan struct{}, doneWg *sync.WaitGroup) {
	doneWg.Go(func() {
		a.run(stopChan)
	})
}

func (a *arpProxyManager) run(stopChan <-chan struct{}) {
	bridgeName := a.gwBridge.GetBridgeName()
	link, err := netlink.LinkByName(bridgeName)
	if err != nil {
		klog.Errorf("ARP proxy: cannot resolve bridge %s ifindex: %v", bridgeName, err)
		return
	}
	a.bridgeIfIndex = link.Attrs().Index

	timer := time.NewTicker(a.syncPeriod)
	defer timer.Stop()

	neighChan := make(chan netlink.NeighUpdate)
	err = netlink.NeighSubscribeWithOptions(neighChan, stopChan, netlink.NeighSubscribeOptions{
		ErrorCallback: func(err error) {
			klog.Errorf("ARP proxy: netlink neighbor subscribe error: %v", err)
		},
		ListExisting: true,
	})
	if err != nil {
		klog.Errorf("ARP proxy: failed to subscribe to neighbor updates, falling back to polling: %v", err)
		a.sync()
	}

	klog.Info("ARP proxy manager is running")
	for {
		select {
		case update, ok := <-neighChan:
			if !ok {
				klog.Warning("ARP proxy: neighbor channel closed, re-subscribing")
				neighChan = make(chan netlink.NeighUpdate)
				if err := netlink.NeighSubscribeWithOptions(neighChan, stopChan, netlink.NeighSubscribeOptions{
					ErrorCallback: func(err error) {
						klog.Errorf("ARP proxy: netlink neighbor subscribe error: %v", err)
					},
					ListExisting: true,
				}); err != nil {
					klog.Errorf("ARP proxy: re-subscribe failed: %v", err)
				}
				continue
			}
			a.handleNeighUpdate(update)
		case <-timer.C:
			a.sync()
		case <-stopChan:
			klog.Info("ARP proxy manager stopping")
			return
		}
	}
}

func (a *arpProxyManager) handleNeighUpdate(update netlink.NeighUpdate) {
	if update.LinkIndex != a.bridgeIfIndex {
		return
	}
	if update.IP == nil || update.IP.To4() == nil {
		return
	}

	ip := update.IP.String()

	a.neighMutex.Lock()
	defer a.neighMutex.Unlock()

	if update.Type == unix.RTM_DELNEIGH || !isReachableState(update.State) {
		if _, exists := a.neighbors[ip]; exists {
			delete(a.neighbors, ip)
			klog.V(5).Infof("ARP proxy: removed neighbor %s", ip)
			a.updateFlowsLocked()
		}
		return
	}

	if len(update.HardwareAddr) == 0 {
		return
	}

	existing, exists := a.neighbors[ip]
	if !exists || existing.String() != update.HardwareAddr.String() {
		a.neighbors[ip] = update.HardwareAddr
		klog.V(5).Infof("ARP proxy: learned neighbor %s -> %s", ip, update.HardwareAddr)
		a.updateFlowsLocked()
	}
}

func (a *arpProxyManager) sync() {
	neighs, err := netlink.NeighList(a.bridgeIfIndex, netlink.FAMILY_V4)
	if err != nil {
		klog.Errorf("ARP proxy: failed to list neighbors: %v", err)
		return
	}

	newNeighbors := make(map[string]net.HardwareAddr)
	for i := range neighs {
		n := &neighs[i]
		if n.IP == nil || n.IP.To4() == nil {
			continue
		}
		if !isReachableState(n.State) {
			continue
		}
		if len(n.HardwareAddr) == 0 {
			continue
		}
		newNeighbors[n.IP.String()] = n.HardwareAddr
	}

	a.neighMutex.Lock()
	defer a.neighMutex.Unlock()

	changed := len(a.neighbors) != len(newNeighbors)
	if !changed {
		for ip, mac := range newNeighbors {
			if existing, ok := a.neighbors[ip]; !ok || existing.String() != mac.String() {
				changed = true
				break
			}
		}
	}

	if changed {
		a.neighbors = newNeighbors
		klog.V(4).Infof("ARP proxy: synced %d neighbors", len(newNeighbors))
		a.updateFlowsLocked()
	}
}

// updateFlowsLocked generates ARP responder flows and updates the flow cache.
// Must be called with neighMutex held.
func (a *arpProxyManager) updateFlowsLocked() {
	var flows []string
	for ip, mac := range a.neighbors {
		flows = append(flows, generateARPResponderFlow(ip, mac.String()))
	}
	a.ofm.updateFlowCacheEntry("ARP_PROXY", flows)
	a.ofm.requestFlowSync()
}

// generateARPResponderFlow creates an OpenFlow rule that answers an ARP request
// for the given IP with the given MAC directly on the bridge, avoiding broadcast.
func generateARPResponderFlow(ip, mac string) string {
	return fmt.Sprintf("cookie=%s, priority=40, table=0, arp, arp_op=1, arp_tpa=%s, "+
		"actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[], "+
		"set_field:%s->eth_src, "+
		"set_field:2->arp_op, "+
		"move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[], "+
		"move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[], "+
		"set_field:%s->arp_sha, "+
		"set_field:%s->arp_spa, "+
		"IN_PORT",
		nodetypes.ARPProxyCookie, ip, mac, mac, ip)
}

func isReachableState(state int) bool {
	return state&(unix.NUD_REACHABLE|unix.NUD_STALE|unix.NUD_DELAY|unix.NUD_PROBE|unix.NUD_PERMANENT) != 0
}
