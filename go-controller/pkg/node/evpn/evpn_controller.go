package evpn

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/vishvananda/netlink"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	controllerutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/controller"
	vtepv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vteplisters "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/listers/vtep/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	controllerName = "evpn-controller"

	// Key prefixes for reconciliation
	vtepKeyPrefix    = "vtep/"
	networkKeyPrefix = "network/"

	// Annotation key for VTEP IP on nodes (set by cluster manager in managed mode)
	// Format: {"vtep-name": "10.0.0.1", "other-vtep": "10.0.0.2"}
	OvnNodeVTEPIPs = "k8s.ovn.org/node-vtep-ips"

	// Device name prefixes for EVPN infrastructure
	dummyPrefix  = "vtep-"
	bridgePrefix = "br-evpn-"
	vxlanPrefix  = "vxevpn-"

	// VXLAN destination port
	vxlanDstPort = 4789

	// Max interface name length
	ifaceNameMaxLen = 15
)

// vtepState holds the current state of a VTEP on this node
type vtepState struct {
	// ready indicates whether the VTEP infrastructure is fully configured
	ready bool
	// ip is the VTEP IP address (discovered or from annotation)
	ip net.IP
	// mode is the VTEP mode (Managed or Unmanaged)
	mode vtepv1.VTEPMode
	// cidrs are the VTEP CIDRs for IP discovery in unmanaged mode
	cidrs []string
}

// networkState holds the current state of a network's EVPN configuration
type networkState struct {
	// vtepName is the VTEP this network uses
	vtepName string
	// configured indicates whether EVPN is configured for this network
	configured bool
	// macVRFVNI is the VNI for MAC-VRF
	macVRFVNI int32
	// macVRFVID is the VID for MAC-VRF
	macVRFVID int
	// ipVRFVNI is the VNI for IP-VRF
	ipVRFVNI int32
	// ipVRFVID is the VID for IP-VRF
	ipVRFVID int
}

// Controller manages EVPN configuration on a node.
// It handles both VTEP infrastructure (bridge, vxlan, dummy interface)
// and per-network EVPN configuration (VID/VNI mappings, OVS ports, SVIs).
type Controller struct {
	// nodeName is the name of this node
	nodeName string

	// watchFactory provides access to informers
	watchFactory *factory.WatchFactory
	// vtepLister is used to get VTEP CRs
	vtepLister vteplisters.VTEPLister
	// nodeLister is used to get node annotations
	nodeLister corelisters.NodeLister
	// networkManager provides network information
	networkManager networkmanager.Interface

	// reconciler handles the main workqueue
	reconciler controllerutil.Reconciler

	// nadReconciler handles NAD change notifications from NetworkManager
	nadReconciler controllerutil.Reconciler
	// nadReconcilerID is the ID returned by NetworkManager when registering
	nadReconcilerID uint64

	// mu protects the state maps
	mu sync.RWMutex
	// vtepStates holds the current state of each VTEP
	vtepStates map[string]*vtepState
	// networkStates holds the current state of each network's EVPN config
	networkStates map[string]*networkState
	// vtepToNetworks maps VTEP names to networks using them (for cascade)
	vtepToNetworks map[string]map[string]struct{}
	// networkToVTEP maps network names to their VTEP (for lookup)
	networkToVTEP map[string]string

	// stopChan signals the controller to stop
	stopChan chan struct{}
}

// NewController creates a new EVPN controller for the given node.
func NewController(
	nodeName string,
	wf *factory.WatchFactory,
	networkManager networkmanager.Interface,
) *Controller {
	c := &Controller{
		nodeName:       nodeName,
		watchFactory:   wf,
		vtepLister:     wf.VTEPInformer().Lister(),
		nodeLister:     wf.NodeCoreInformer().Lister(),
		networkManager: networkManager,

		vtepStates:     make(map[string]*vtepState),
		networkStates:  make(map[string]*networkState),
		vtepToNetworks: make(map[string]map[string]struct{}),
		networkToVTEP:  make(map[string]string),

		stopChan: make(chan struct{}),
	}

	c.reconciler = controllerutil.NewReconciler(
		controllerName,
		&controllerutil.ReconcilerConfig{
			Threadiness: 1,
			Reconcile:   c.reconcile,
			RateLimiter: workqueue.NewTypedItemFastSlowRateLimiter[string](
				200*time.Millisecond,
				5*time.Second,
				5,
			),
		},
	)

	// NAD reconciler receives NAD keys from NetworkManager and translates them
	// to network keys for the main reconciler
	c.nadReconciler = controllerutil.NewReconciler(
		controllerName+"-nad",
		&controllerutil.ReconcilerConfig{
			Threadiness: 1,
			Reconcile:   c.syncNAD,
			RateLimiter: workqueue.DefaultTypedControllerRateLimiter[string](),
		},
	)

	return c
}

// Start starts the EVPN controller.
func (c *Controller) Start() error {
	if !util.IsRouteAdvertisementsEnabled() {
		klog.Infof("OVN-Kubernetes EVPN controller is not enabled")
		return nil
	}
	klog.Infof("Starting EVPN controller for node %s", c.nodeName)

	// Register for NAD changes via NetworkManager
	var err error
	c.nadReconcilerID, err = c.networkManager.RegisterNADReconciler(c.nadReconciler)
	if err != nil {
		return fmt.Errorf("failed to register NAD reconciler: %w", err)
	}

	// Add event handlers for VTEP CR and Node
	if err := c.setupEventHandlers(); err != nil {
		return fmt.Errorf("failed to setup event handlers: %w", err)
	}

	// Start the reconcilers
	if err := controllerutil.Start(c.reconciler, c.nadReconciler); err != nil {
		return fmt.Errorf("failed to start reconcilers: %w", err)
	}

	// Initial sync: queue all known VTEPs
	vteps, err := c.vtepLister.List(labels.Everything())
	if err != nil {
		klog.Warningf("Failed to list VTEPs for initial sync: %v", err)
	} else {
		for _, vtep := range vteps {
			c.reconciler.Reconcile(vtepKeyPrefix + vtep.Name)
		}
	}

	klog.Infof("EVPN controller started for node %s", c.nodeName)
	return nil
}

// setupEventHandlers sets up informer event handlers for VTEP CR and Node.
func (c *Controller) setupEventHandlers() error {
	// Watch node for VTEP IP annotation changes
	_, err := c.watchFactory.NodeCoreInformer().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode := oldObj.(*corev1.Node)
			newNode := newObj.(*corev1.Node)
			if newNode.Name != c.nodeName {
				return
			}
			if vtepAnnotationsChanged(oldNode.Annotations, newNode.Annotations) {
				klog.V(5).Infof("EVPN controller: Node VTEP annotations changed for node %s", newNode.Name)
				// Queue all VTEPs in managed mode for reconciliation
				c.mu.RLock()
				for vtepName, state := range c.vtepStates {
					if state.mode == vtepv1.VTEPModeManaged {
						c.reconciler.Reconcile(vtepKeyPrefix + vtepName)
					}
				}
				c.mu.RUnlock()
			}
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add node event handler: %w", err)
	}

	// Watch VTEP CRs
	_, err = c.watchFactory.VTEPInformer().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			vtep := obj.(*vtepv1.VTEP)
			klog.V(5).Infof("EVPN controller: VTEP added: %s", vtep.Name)
			c.reconciler.Reconcile(vtepKeyPrefix + vtep.Name)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldVTEP := oldObj.(*vtepv1.VTEP)
			newVTEP := newObj.(*vtepv1.VTEP)
			if oldVTEP.Spec.Mode != newVTEP.Spec.Mode || !cidrsEqual(oldVTEP.Spec.CIDRs, newVTEP.Spec.CIDRs) {
				klog.V(5).Infof("EVPN controller: VTEP updated: %s", newVTEP.Name)
				c.reconciler.Reconcile(vtepKeyPrefix + newVTEP.Name)
			}
		},
		DeleteFunc: func(obj interface{}) {
			vtep, ok := obj.(*vtepv1.VTEP)
			if !ok {
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					vtep, _ = tombstone.Obj.(*vtepv1.VTEP)
				}
			}
			if vtep != nil {
				klog.V(5).Infof("EVPN controller: VTEP deleted: %s", vtep.Name)
				c.reconciler.Reconcile(vtepKeyPrefix + vtep.Name)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add VTEP event handler: %w", err)
	}

	return nil
}

// vtepAnnotationsChanged checks if any VTEP-related annotations changed.
func vtepAnnotationsChanged(oldAnn, newAnn map[string]string) bool {
	oldVal := oldAnn[OvnNodeVTEPIPs]
	newVal := newAnn[OvnNodeVTEPIPs]
	return oldVal != newVal
}

// Stop stops the EVPN controller.
func (c *Controller) Stop() {
	klog.Infof("Stopping EVPN controller for node %s", c.nodeName)

	close(c.stopChan)

	if c.nadReconcilerID != 0 {
		if err := c.networkManager.DeRegisterNADReconciler(c.nadReconcilerID); err != nil {
			klog.Warningf("Failed to deregister NAD reconciler: %v", err)
		}
	}

	controllerutil.Stop(c.reconciler, c.nadReconciler)

	klog.Infof("EVPN controller stopped for node %s", c.nodeName)
}

// syncNAD is called by the NAD reconciler when NetworkManager notifies of NAD changes.
// It translates NAD keys to network keys for the main reconciler.
func (c *Controller) syncNAD(key string) error {
	// NAD key is namespace/name, but we use network name as our key
	// NetworkManager gives us the NAD key; we need to find the network name
	netInfo := c.networkManager.GetNetInfoForNADKey(key)
	if netInfo == nil {
		// NAD deleted or not an EVPN network
		return nil
	}

	networkName := netInfo.GetNetworkName()
	vtepName := netInfo.EVPNVTEPName()

	if vtepName == "" {
		// Not an EVPN network, check if we need to clean up
		c.mu.Lock()
		if _, exists := c.networkStates[networkName]; exists {
			delete(c.networkStates, networkName)
			c.mu.Unlock()
			c.reconciler.Reconcile(networkKeyPrefix + networkName)
		} else {
			c.mu.Unlock()
		}
		return nil
	}

	// Queue network for reconciliation
	c.reconciler.Reconcile(networkKeyPrefix + networkName)
	return nil
}

// reconcile is the main reconciliation function.
// It handles both VTEP and network reconciliation based on the key prefix.
func (c *Controller) reconcile(key string) error {
	startTime := time.Now()
	defer func() {
		klog.V(5).Infof("EVPN controller: reconciled %q in %v", key, time.Since(startTime))
	}()

	switch {
	case strings.HasPrefix(key, vtepKeyPrefix):
		vtepName := strings.TrimPrefix(key, vtepKeyPrefix)
		return c.reconcileVTEP(vtepName)

	case strings.HasPrefix(key, networkKeyPrefix):
		networkName := strings.TrimPrefix(key, networkKeyPrefix)
		return c.reconcileNetwork(networkName)

	default:
		klog.Warningf("EVPN controller: unknown key format: %q", key)
		return nil
	}
}

// reconcileVTEP ensures VTEP infrastructure is configured.
func (c *Controller) reconcileVTEP(vtepName string) error {
	klog.V(4).Infof("EVPN controller: reconciling VTEP %q", vtepName)

	vtep, err := c.vtepLister.Get(vtepName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.cleanupVTEP(vtepName)
		}
		return fmt.Errorf("failed to get VTEP %q: %w", vtepName, err)
	}

	// Determine VTEP IP based on mode
	var vtepIP net.IP
	mode := vtep.Spec.Mode
	if mode == "" {
		mode = vtepv1.VTEPModeManaged
	}

	cidrs := make([]string, len(vtep.Spec.CIDRs))
	for i, cidr := range vtep.Spec.CIDRs {
		cidrs[i] = string(cidr)
	}

	switch mode {
	case vtepv1.VTEPModeManaged:
		vtepIP, err = c.getVTEPIPFromNodeAnnotation(vtepName)
		if err != nil {
			klog.V(4).Infof("EVPN controller: VTEP %q waiting for IP allocation: %v", vtepName, err)
			c.setVTEPNotReady(vtepName, mode, cidrs)
			// Will be requeued when node annotation changes
			return nil
		}

		// Ensure dummy interface with VTEP IP
		if err := c.ensureDummyInterface(vtepName, vtepIP); err != nil {
			return fmt.Errorf("failed to ensure dummy interface for VTEP %q: %w", vtepName, err)
		}

	case vtepv1.VTEPModeUnmanaged:
		vtepIP = c.discoverVTEPIP(cidrs)
		if vtepIP == nil {
			klog.V(4).Infof("EVPN controller: VTEP %q waiting for IP to appear on host", vtepName)
			c.setVTEPNotReady(vtepName, mode, cidrs)
			// Will be requeued when netlink address event occurs
			return nil
		}
		// No dummy interface needed - IP already exists externally

	default:
		return fmt.Errorf("unknown VTEP mode %q for VTEP %q", mode, vtepName)
	}

	// Ensure bridge
	if err := c.ensureBridge(vtepName); err != nil {
		return fmt.Errorf("failed to ensure bridge for VTEP %q: %w", vtepName, err)
	}

	// Ensure VXLAN
	if err := c.ensureVXLAN(vtepName, vtepIP); err != nil {
		return fmt.Errorf("failed to ensure VXLAN for VTEP %q: %w", vtepName, err)
	}

	// Mark VTEP as ready and cascade to dependent networks
	c.setVTEPReady(vtepName, vtepIP, mode, cidrs)

	// Cascade: reconcile all networks using this VTEP
	c.mu.RLock()
	networks := c.vtepToNetworks[vtepName]
	c.mu.RUnlock()

	for networkName := range networks {
		c.reconciler.Reconcile(networkKeyPrefix + networkName)
	}

	klog.V(4).Infof("EVPN controller: VTEP %q ready with IP %s", vtepName, vtepIP)
	return nil
}

// reconcileNetwork ensures per-network EVPN configuration.
func (c *Controller) reconcileNetwork(networkName string) error {
	klog.V(4).Infof("EVPN controller: reconciling network %q", networkName)

	netInfo := c.networkManager.GetNetwork(networkName)
	if netInfo == nil {
		return c.cleanupNetwork(networkName)
	}

	vtepName := netInfo.EVPNVTEPName()
	if vtepName == "" {
		// Not an EVPN network
		return c.cleanupNetwork(networkName)
	}

	// Update indexes
	c.mu.Lock()
	oldVTEP := c.networkToVTEP[networkName]
	if oldVTEP != "" && oldVTEP != vtepName {
		// Network switched VTEPs, remove from old
		if networks, ok := c.vtepToNetworks[oldVTEP]; ok {
			delete(networks, networkName)
		}
	}
	c.networkToVTEP[networkName] = vtepName
	if c.vtepToNetworks[vtepName] == nil {
		c.vtepToNetworks[vtepName] = make(map[string]struct{})
	}
	c.vtepToNetworks[vtepName][networkName] = struct{}{}
	c.mu.Unlock()

	// Check if VTEP is ready
	if !c.isVTEPReady(vtepName) {
		klog.V(4).Infof("EVPN controller: network %q waiting for VTEP %q to be ready", networkName, vtepName)
		// Ensure VTEP is queued for reconciliation
		c.reconciler.Reconcile(vtepKeyPrefix + vtepName)
		// Will be reconciled when VTEP becomes ready (via cascade)
		return nil
	}

	// Get EVPN configuration from network info
	macVRFVNI := netInfo.EVPNMACVRFVNI()
	macVRFVID := netInfo.EVPNMACVRFVID()
	ipVRFVNI := netInfo.EVPNIPVRFVNI()
	ipVRFVID := netInfo.EVPNIPVRFVID()

	// Configure VID/VNI mappings
	if macVRFVNI > 0 {
		if err := c.configureVIDVNI(vtepName, macVRFVID, int(macVRFVNI)); err != nil {
			return fmt.Errorf("failed to configure MAC-VRF VID/VNI for network %q: %w", networkName, err)
		}
	}

	if ipVRFVNI > 0 {
		if err := c.configureVIDVNI(vtepName, ipVRFVID, int(ipVRFVNI)); err != nil {
			return fmt.Errorf("failed to configure IP-VRF VID/VNI for network %q: %w", networkName, err)
		}
	}

	// Setup OVS port for MAC-VRF
	if macVRFVNI > 0 {
		if err := c.setupOVSPort(networkName, vtepName, macVRFVID); err != nil {
			return fmt.Errorf("failed to setup OVS port for network %q: %w", networkName, err)
		}
	}

	// Setup SVI for IP-VRF
	if ipVRFVNI > 0 {
		if err := c.setupSVI(networkName, vtepName, ipVRFVID); err != nil {
			return fmt.Errorf("failed to setup SVI for network %q: %w", networkName, err)
		}
	}

	// Update network state
	c.mu.Lock()
	c.networkStates[networkName] = &networkState{
		vtepName:   vtepName,
		configured: true,
		macVRFVNI:  macVRFVNI,
		macVRFVID:  macVRFVID,
		ipVRFVNI:   ipVRFVNI,
		ipVRFVID:   ipVRFVID,
	}
	c.mu.Unlock()

	klog.V(4).Infof("EVPN controller: network %q configured (MAC-VRF VNI=%d VID=%d, IP-VRF VNI=%d VID=%d)",
		networkName, macVRFVNI, macVRFVID, ipVRFVNI, ipVRFVID)
	return nil
}

// cleanupVTEP removes VTEP infrastructure.
func (c *Controller) cleanupVTEP(vtepName string) error {
	klog.V(4).Infof("EVPN controller: cleaning up VTEP %q", vtepName)

	c.mu.Lock()
	delete(c.vtepStates, vtepName)
	networks := c.vtepToNetworks[vtepName]
	delete(c.vtepToNetworks, vtepName)
	c.mu.Unlock()

	// Cleanup networks first
	for networkName := range networks {
		if err := c.cleanupNetwork(networkName); err != nil {
			klog.Warningf("EVPN controller: failed to cleanup network %q during VTEP cleanup: %v", networkName, err)
		}
	}

	// Cleanup VTEP infrastructure (stubbed)
	if err := c.deleteVXLAN(vtepName); err != nil {
		klog.Warningf("EVPN controller: failed to delete VXLAN for VTEP %q: %v", vtepName, err)
	}
	if err := c.deleteBridge(vtepName); err != nil {
		klog.Warningf("EVPN controller: failed to delete bridge for VTEP %q: %v", vtepName, err)
	}
	if err := c.deleteDummyInterface(vtepName); err != nil {
		klog.Warningf("EVPN controller: failed to delete dummy interface for VTEP %q: %v", vtepName, err)
	}

	klog.V(4).Infof("EVPN controller: VTEP %q cleaned up", vtepName)
	return nil
}

// cleanupNetwork removes per-network EVPN configuration.
func (c *Controller) cleanupNetwork(networkName string) error {
	c.mu.Lock()
	state, exists := c.networkStates[networkName]
	if !exists {
		c.mu.Unlock()
		return nil
	}

	vtepName := c.networkToVTEP[networkName]
	delete(c.networkStates, networkName)
	delete(c.networkToVTEP, networkName)
	if networks, ok := c.vtepToNetworks[vtepName]; ok {
		delete(networks, networkName)
	}
	c.mu.Unlock()

	klog.V(4).Infof("EVPN controller: cleaning up network %q", networkName)

	// Cleanup SVI
	if state.ipVRFVNI > 0 {
		if err := c.deleteSVI(networkName, vtepName); err != nil {
			klog.Warningf("EVPN controller: failed to delete SVI for network %q: %v", networkName, err)
		}
	}

	// Cleanup OVS port
	if state.macVRFVNI > 0 {
		if err := c.deleteOVSPort(networkName, vtepName); err != nil {
			klog.Warningf("EVPN controller: failed to delete OVS port for network %q: %v", networkName, err)
		}
	}

	// Cleanup VID/VNI mappings
	if state.macVRFVNI > 0 {
		if err := c.cleanupVIDVNI(vtepName, state.macVRFVID, int(state.macVRFVNI)); err != nil {
			klog.Warningf("EVPN controller: failed to cleanup MAC-VRF VID/VNI for network %q: %v", networkName, err)
		}
	}
	if state.ipVRFVNI > 0 {
		if err := c.cleanupVIDVNI(vtepName, state.ipVRFVID, int(state.ipVRFVNI)); err != nil {
			klog.Warningf("EVPN controller: failed to cleanup IP-VRF VID/VNI for network %q: %v", networkName, err)
		}
	}

	klog.V(4).Infof("EVPN controller: network %q cleaned up", networkName)
	return nil
}



func (c *Controller) setVTEPReady(vtepName string, ip net.IP, mode vtepv1.VTEPMode, cidrs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vtepStates[vtepName] = &vtepState{
		ready: true,
		ip:    ip,
		mode:  mode,
		cidrs: cidrs,
	}
}

func (c *Controller) setVTEPNotReady(vtepName string, mode vtepv1.VTEPMode, cidrs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vtepStates[vtepName] = &vtepState{
		ready: false,
		mode:  mode,
		cidrs: cidrs,
	}
}

func (c *Controller) isVTEPReady(vtepName string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	state, exists := c.vtepStates[vtepName]
	return exists && state.ready
}

// --- VTEP IP discovery ---

// getVTEPIPFromNodeAnnotation reads the VTEP IP from node annotation (managed mode).
func (c *Controller) getVTEPIPFromNodeAnnotation(vtepName string) (net.IP, error) {
	node, err := c.nodeLister.Get(c.nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node %q: %w", c.nodeName, err)
	}

	annotation := node.Annotations[OvnNodeVTEPIPs]
	if annotation == "" {
		return nil, fmt.Errorf("VTEP IP annotation not found on node")
	}

	// Parse JSON map: {"vtep-name": "10.0.0.1", ...}
	vtepIPs := make(map[string]string)
	if err := json.Unmarshal([]byte(annotation), &vtepIPs); err != nil {
		return nil, fmt.Errorf("failed to parse VTEP IPs annotation: %w", err)
	}

	ipStr, ok := vtepIPs[vtepName]
	if !ok || ipStr == "" {
		return nil, fmt.Errorf("VTEP IP for %q not found in annotation", vtepName)
	}

	ip := net.ParseIP(ipStr)
	if ip == nil {
		return nil, fmt.Errorf("invalid VTEP IP %q for %q", ipStr, vtepName)
	}

	return ip, nil
}

// discoverVTEPIP scans host interfaces for an IP in the VTEP CIDRs (unmanaged mode).
func (c *Controller) discoverVTEPIP(cidrs []string) net.IP {
	// Parse CIDRs
	var nets []*net.IPNet
	for _, cidrStr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidrStr)
		if err != nil {
			klog.Warningf("EVPN controller: invalid CIDR %q: %v", cidrStr, err)
			continue
		}
		nets = append(nets, ipNet)
	}
	if len(nets) == 0 {
		return nil
	}

	// List all links and their addresses
	links, err := util.GetNetLinkOps().LinkList()
	if err != nil {
		klog.Warningf("EVPN controller: failed to list links: %v", err)
		return nil
	}

	var matches []net.IP
	for _, link := range links {
		addrs, err := util.GetNetLinkOps().AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			for _, ipNet := range nets {
				if ipNet.Contains(addr.IP) {
					matches = append(matches, addr.IP)
					break
				}
			}
		}
	}

	if len(matches) == 0 {
		return nil
	}

	// Return first IP (deterministic ordering)
	slices.SortFunc(matches, func(a, b net.IP) int {
		return slices.Compare(a.To16(), b.To16())
	})
	return matches[0]
}

// --- Infrastructure methods ---

// ensureDummyInterface creates/updates the dummy interface with VTEP IP.
func (c *Controller) ensureDummyInterface(vtepName string, ip net.IP) error {
	deviceName := formatDeviceName(dummyPrefix, vtepName)

	link, err := util.GetNetLinkOps().LinkByName(deviceName)
	if err != nil {
		if !util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return fmt.Errorf("failed to get dummy interface: %w", err)
		}

		// Create dummy interface
		dummy := &netlink.Dummy{
			LinkAttrs: netlink.LinkAttrs{Name: deviceName},
		}
		if err := util.GetNetLinkOps().LinkAdd(dummy); err != nil {
			return fmt.Errorf("failed to create dummy interface: %w", err)
		}

		link, err = util.GetNetLinkOps().LinkByName(deviceName)
		if err != nil {
			return fmt.Errorf("failed to get created dummy interface: %w", err)
		}
	}

	// Ensure interface is up
	if link.Attrs().Flags&net.FlagUp == 0 {
		if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
			return fmt.Errorf("failed to set dummy interface up: %w", err)
		}
	}

	// Ensure IP is configured
	prefixLen := 32
	if ip.To4() == nil {
		prefixLen = 128
	}
	addr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   ip,
			Mask: net.CIDRMask(prefixLen, prefixLen),
		},
	}

	addrs, err := util.GetNetLinkOps().AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("failed to list addresses on dummy interface: %w", err)
	}

	// Check if address already exists
	found := false
	for _, a := range addrs {
		if a.IP.Equal(ip) {
			found = true
			break
		}
	}

	if !found {
		if err := util.GetNetLinkOps().AddrAdd(link, addr); err != nil {
			return fmt.Errorf("failed to add IP to dummy interface: %w", err)
		}
		klog.Infof("EVPN controller: added IP %s to dummy interface %s", ip, deviceName)
	}

	return nil
}

// deleteDummyInterface removes the dummy interface.
func (c *Controller) deleteDummyInterface(vtepName string) error {
	deviceName := formatDeviceName(dummyPrefix, vtepName)

	link, err := util.GetNetLinkOps().LinkByName(deviceName)
	if err != nil {
		if util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return nil // Already gone
		}
		return fmt.Errorf("failed to get dummy interface: %w", err)
	}

	if err := util.GetNetLinkOps().LinkDelete(link); err != nil {
		return fmt.Errorf("failed to delete dummy interface: %w", err)
	}

	klog.Infof("EVPN controller: deleted dummy interface %s", deviceName)
	return nil
}

// ensureBridge creates/updates the EVPN bridge with VLAN filtering enabled.
func (c *Controller) ensureBridge(vtepName string) error {
	deviceName := formatDeviceName(bridgePrefix, vtepName)

	link, err := util.GetNetLinkOps().LinkByName(deviceName)
	if err != nil {
		if !util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return fmt.Errorf("failed to get bridge: %w", err)
		}

		// Create bridge with VLAN filtering enabled
		vlanFiltering := true
		vlanDefaultPVID := uint16(0) // No default PVID
		bridge := &netlink.Bridge{
			LinkAttrs:       netlink.LinkAttrs{Name: deviceName},
			VlanFiltering:   &vlanFiltering,
			VlanDefaultPVID: &vlanDefaultPVID,
		}

		if err := util.GetNetLinkOps().LinkAdd(bridge); err != nil {
			return fmt.Errorf("failed to create bridge: %w", err)
		}

		link, err = util.GetNetLinkOps().LinkByName(deviceName)
		if err != nil {
			return fmt.Errorf("failed to get created bridge: %w", err)
		}

		klog.Infof("EVPN controller: created bridge %s", deviceName)
	} else if _, ok := link.(*netlink.Bridge); !ok {
		return fmt.Errorf("link %s exists but is not a bridge", deviceName)
	}

	// Ensure bridge is up
	if link.Attrs().Flags&net.FlagUp == 0 {
		if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
			return fmt.Errorf("failed to set bridge up: %w", err)
		}
	}

	return nil
}

// deleteBridge removes the EVPN bridge.
func (c *Controller) deleteBridge(vtepName string) error {
	deviceName := formatDeviceName(bridgePrefix, vtepName)

	link, err := util.GetNetLinkOps().LinkByName(deviceName)
	if err != nil {
		if util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return nil // Already gone
		}
		return fmt.Errorf("failed to get bridge: %w", err)
	}

	if err := util.GetNetLinkOps().LinkDelete(link); err != nil {
		return fmt.Errorf("failed to delete bridge: %w", err)
	}

	klog.Infof("EVPN controller: deleted bridge %s", deviceName)
	return nil
}

// ensureVXLAN creates/updates the VXLAN device in FlowBased/VniFilter mode and attaches to bridge.
func (c *Controller) ensureVXLAN(vtepName string, vtepIP net.IP) error {
	vxlanName := formatDeviceName(vxlanPrefix, vtepName)
	bridgeName := formatDeviceName(bridgePrefix, vtepName)

	bridgeLink, err := util.GetNetLinkOps().LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("bridge %s must exist before VXLAN: %w", bridgeName, err)
	}

	link, err := util.GetNetLinkOps().LinkByName(vxlanName)
	if err != nil {
		if !util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return fmt.Errorf("failed to get VXLAN: %w", err)
		}

		link, err = c.createVXLAN(vxlanName, vtepIP)
		if err != nil {
			return err
		}
		klog.Infof("EVPN controller: created VXLAN %s with source IP %s", vxlanName, vtepIP)
	} else {
		vxlan, ok := link.(*netlink.Vxlan)
		if !ok {
			return fmt.Errorf("link %s exists but is not a VXLAN device", vxlanName)
		}
		// Recreate if config changed (source IP or port)
		if !vxlan.SrcAddr.Equal(vtepIP) || vxlan.Port != vxlanDstPort {
			if err := util.GetNetLinkOps().LinkDelete(link); err != nil {
				return fmt.Errorf("failed to delete VXLAN for update: %w", err)
			}
			link, err = c.createVXLAN(vxlanName, vtepIP)
			if err != nil {
				return err
			}
			klog.Infof("EVPN controller: recreated VXLAN %s with new source IP %s", vxlanName, vtepIP)
		}
	}

	// Attach VXLAN to bridge if not already
	if link.Attrs().MasterIndex != bridgeLink.Attrs().Index {
		if err := util.GetNetLinkOps().LinkSetMaster(link, bridgeLink); err != nil {
			return fmt.Errorf("failed to attach VXLAN to bridge: %w", err)
		}
	}

	// Configure VXLAN bridge port settings
	if err := c.configureVXLANBridgePort(link); err != nil {
		return fmt.Errorf("failed to configure VXLAN bridge port: %w", err)
	}

	// Ensure VXLAN is up
	if link.Attrs().Flags&net.FlagUp == 0 {
		if err := util.GetNetLinkOps().LinkSetUp(link); err != nil {
			return fmt.Errorf("failed to set VXLAN up: %w", err)
		}
	}

	return nil
}

// createVXLAN creates a new VXLAN device in FlowBased/VniFilter mode.
func (c *Controller) createVXLAN(name string, localIP net.IP) (netlink.Link, error) {
	// Use FlowBased (external) mode with VniFilter for multi-VNI EVPN support
	vxlan := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{Name: name},
		FlowBased: true, // External mode required for VNI filtering
		VniFilter: true, // Enable VNI filtering for per-VNI configuration
		Port:      vxlanDstPort,
		Learning:  false,
		SrcAddr:   localIP,
	}

	if err := util.GetNetLinkOps().LinkAdd(vxlan); err != nil {
		return nil, fmt.Errorf("failed to create VXLAN: %w", err)
	}

	link, err := util.GetNetLinkOps().LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("failed to get created VXLAN: %w", err)
	}
	return link, nil
}

// configureVXLANBridgePort configures VXLAN bridge port settings for EVPN.
func (c *Controller) configureVXLANBridgePort(link netlink.Link) error {
	// Enable vlan_tunnel to map VLANs to VNIs
	if err := util.GetNetLinkOps().LinkSetVlanTunnel(link, true); err != nil {
		return fmt.Errorf("failed to set vlan_tunnel: %w", err)
	}
	// Enable neigh_suppress to suppress ARP/ND on VXLAN
	if err := util.GetNetLinkOps().LinkSetBrNeighSuppress(link, true); err != nil {
		return fmt.Errorf("failed to set neigh_suppress: %w", err)
	}
	// Disable learning on VXLAN port
	if err := util.GetNetLinkOps().LinkSetLearning(link, false); err != nil {
		return fmt.Errorf("failed to disable learning: %w", err)
	}
	return nil
}

// deleteVXLAN removes the VXLAN device.
func (c *Controller) deleteVXLAN(vtepName string) error {
	deviceName := formatDeviceName(vxlanPrefix, vtepName)

	link, err := util.GetNetLinkOps().LinkByName(deviceName)
	if err != nil {
		if util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return nil // Already gone
		}
		return fmt.Errorf("failed to get VXLAN: %w", err)
	}

	if err := util.GetNetLinkOps().LinkDelete(link); err != nil {
		return fmt.Errorf("failed to delete VXLAN: %w", err)
	}

	klog.Infof("EVPN controller: deleted VXLAN %s", deviceName)
	return nil
}

// configureVIDVNI adds VID/VNI mapping to bridge and VXLAN.
func (c *Controller) configureVIDVNI(vtepName string, vid int, vni int) error {
	bridgeName := formatDeviceName(bridgePrefix, vtepName)
	vxlanName := formatDeviceName(vxlanPrefix, vtepName)

	bridgeLink, err := util.GetNetLinkOps().LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("failed to get bridge: %w", err)
	}

	vxlanLink, err := util.GetNetLinkOps().LinkByName(vxlanName)
	if err != nil {
		return fmt.Errorf("failed to get VXLAN: %w", err)
	}

	vid16 := uint16(vid)
	vni32 := uint32(vni)

	// Add VID to bridge (self)
	if err := util.GetNetLinkOps().BridgeVlanAdd(bridgeLink, vid16, false, false, true, false); err != nil {
		if !isExistsError(err) {
			return fmt.Errorf("failed to add VID %d to bridge: %w", vid, err)
		}
		klog.V(5).Infof("EVPN controller: VID %d already exists on bridge (self)", vid)
	} else {
		klog.V(5).Infof("EVPN controller: added VID %d to bridge (self)", vid)
	}

	// Add VID to VXLAN (master)
	if err := util.GetNetLinkOps().BridgeVlanAdd(vxlanLink, vid16, false, false, false, true); err != nil {
		if !isExistsError(err) {
			return fmt.Errorf("failed to add VID %d to VXLAN: %w", vid, err)
		}
		klog.V(5).Infof("EVPN controller: VID %d already exists on VXLAN (master)", vid)
	} else {
		klog.V(5).Infof("EVPN controller: added VID %d to VXLAN (master)", vid)
	}

	// Add VNI to VNI filter (bridge vni add dev vxlan0 vni $vni)
	if err := util.GetNetLinkOps().BridgeVniAdd(vxlanLink, vni32); err != nil {
		if !isExistsError(err) {
			return fmt.Errorf("failed to add VNI %d to VXLAN: %w", vni, err)
		}
		klog.V(5).Infof("EVPN controller: VNI %d already exists in VNI filter", vni)
	} else {
		klog.Infof("EVPN controller: added VNI %d to VNI filter on %s", vni, vxlanName)
	}

	// Add tunnel info (VID -> VNI mapping)
	if err := util.GetNetLinkOps().BridgeVlanAddTunnelInfo(vxlanLink, vid16, vni32, false, true); err != nil {
		if !isExistsError(err) {
			return fmt.Errorf("failed to add tunnel info VID %d VNI %d: %w", vid, vni, err)
		}
		klog.V(5).Infof("EVPN controller: tunnel info VID %d -> VNI %d already exists", vid, vni)
	} else {
		klog.Infof("EVPN controller: added tunnel mapping VID %d -> VNI %d on %s", vid, vni, vxlanName)
	}

	return nil
}

// cleanupVIDVNI removes VID/VNI mapping from bridge and VXLAN.
func (c *Controller) cleanupVIDVNI(vtepName string, vid int, vni int) error {
	vxlanName := formatDeviceName(vxlanPrefix, vtepName)

	vxlanLink, err := util.GetNetLinkOps().LinkByName(vxlanName)
	if err != nil {
		if util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return nil // VXLAN already gone
		}
		return fmt.Errorf("failed to get VXLAN: %w", err)
	}

	vid16 := uint16(vid)
	vni32 := uint32(vni)

	// Remove tunnel info (VID -> VNI mapping)
	if err := util.GetNetLinkOps().BridgeVlanDelTunnelInfo(vxlanLink, vid16, vni32, false, true); err != nil {
		klog.V(5).Infof("EVPN controller: failed to remove tunnel info VID %d VNI %d: %v", vid, vni, err)
	} else {
		klog.Infof("EVPN controller: removed tunnel mapping VID %d -> VNI %d on %s", vid, vni, vxlanName)
	}

	// Remove VNI from VNI filter
	if err := util.GetNetLinkOps().BridgeVniDel(vxlanLink, vni32); err != nil {
		klog.V(5).Infof("EVPN controller: failed to remove VNI %d from VNI filter: %v", vni, err)
	} else {
		klog.Infof("EVPN controller: removed VNI %d from VNI filter on %s", vni, vxlanName)
	}

	// Note: We don't remove VID from VXLAN/bridge here as it may be shared with other networks
	// VID cleanup will happen when VTEP is deleted

	return nil
}

// setupOVSPort creates the OVS internal port for MAC-VRF and attaches to EVPN bridge.
func (c *Controller) setupOVSPort(networkName, vtepName string, vid int) error {
	portName := formatDeviceName("evpn-", networkName)
	bridgeName := formatDeviceName(bridgePrefix, vtepName)

	klog.V(4).Infof("EVPN controller: setting up OVS port %s for network %q, VID %d", portName, networkName, vid)

	// Create OVS internal port on br-int
	stdout, stderr, err := util.RunOVSVsctl(
		"--may-exist", "add-port", "br-int", portName,
		"--", "set", "interface", portName, "type=internal",
		fmt.Sprintf("external-ids:iface-id=%s", portName),
	)
	if err != nil {
		return fmt.Errorf("failed to create OVS port %s: %v (stdout: %s, stderr: %s)", portName, err, stdout, stderr)
	}

	// Get the port link
	portLink, err := util.GetNetLinkOps().LinkByName(portName)
	if err != nil {
		return fmt.Errorf("failed to get OVS port %s: %w", portName, err)
	}

	// Get the EVPN bridge
	bridgeLink, err := util.GetNetLinkOps().LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("failed to get bridge: %w", err)
	}

	// Attach to EVPN bridge
	if portLink.Attrs().MasterIndex != bridgeLink.Attrs().Index {
		if err := util.GetNetLinkOps().LinkSetMaster(portLink, bridgeLink); err != nil {
			return fmt.Errorf("failed to attach port to bridge: %w", err)
		}
	}

	// Configure VLAN access (pvid untagged)
	vid16 := uint16(vid)
	if err := util.GetNetLinkOps().BridgeVlanAdd(portLink, vid16, true, true, false, true); err != nil {
		if !isExistsError(err) {
			klog.Warningf("EVPN controller: failed to configure VLAN access for port %s: %v", portName, err)
		}
	}

	// Ensure port is up
	if portLink.Attrs().Flags&net.FlagUp == 0 {
		if err := util.GetNetLinkOps().LinkSetUp(portLink); err != nil {
			return fmt.Errorf("failed to set port up: %w", err)
		}
	}

	klog.Infof("EVPN controller: OVS port %s configured with VID %d", portName, vid)
	return nil
}

// deleteOVSPort removes the OVS port for MAC-VRF.
func (c *Controller) deleteOVSPort(networkName, _ string) error {
	portName := formatDeviceName("evpn-", networkName)

	// Get port link and remove from bridge
	portLink, err := util.GetNetLinkOps().LinkByName(portName)
	if err == nil {
		// Remove VLAN access and detach from bridge
		_ = util.GetNetLinkOps().LinkSetNoMaster(portLink)
	}

	// Remove OVS port
	_, _, _ = util.RunOVSVsctl("--if-exists", "del-port", "br-int", portName)

	klog.Infof("EVPN controller: deleted OVS port %s", portName)
	return nil
}

// setupSVI creates the SVI (L3 interface) for IP-VRF.
func (c *Controller) setupSVI(networkName, vtepName string, vid int) error {
	sviName := formatSVIName(vtepName, vid)
	bridgeName := formatDeviceName(bridgePrefix, vtepName)

	klog.V(4).Infof("EVPN controller: setting up SVI %s for network %q, VID %d", sviName, networkName, vid)

	bridgeLink, err := util.GetNetLinkOps().LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("failed to get bridge: %w", err)
	}

	sviLink, err := util.GetNetLinkOps().LinkByName(sviName)
	if err != nil {
		if !util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return fmt.Errorf("failed to get SVI: %w", err)
		}

		// Create VLAN interface on bridge
		vlan := &netlink.Vlan{
			LinkAttrs:    netlink.LinkAttrs{Name: sviName, ParentIndex: bridgeLink.Attrs().Index},
			VlanId:       vid,
			VlanProtocol: netlink.VLAN_PROTOCOL_8021Q,
		}

		if err := util.GetNetLinkOps().LinkAdd(vlan); err != nil {
			return fmt.Errorf("failed to create SVI: %w", err)
		}

		sviLink, err = util.GetNetLinkOps().LinkByName(sviName)
		if err != nil {
			return fmt.Errorf("failed to get created SVI: %w", err)
		}

		// Set deterministic MAC
		mac := generateMAC(networkName)
		if err := util.GetNetLinkOps().LinkSetHardwareAddr(sviLink, mac); err != nil {
			klog.Warningf("EVPN controller: failed to set MAC for SVI %s: %v", sviName, err)
		}

		klog.Infof("EVPN controller: created SVI %s", sviName)
	}

	// Attach to VRF
	netInfo := c.networkManager.GetNetwork(networkName)
	if netInfo == nil {
		return fmt.Errorf("network %s not found in network manager", networkName)
	}
	vrfName := util.GetNetworkVRFName(netInfo)
	vrfLink, err := util.GetNetLinkOps().LinkByName(vrfName)
	if err != nil {
		return fmt.Errorf("VRF %s does not exist: %w", vrfName, err)
	}

	if sviLink.Attrs().MasterIndex != vrfLink.Attrs().Index {
		if err := util.GetNetLinkOps().LinkSetMaster(sviLink, vrfLink); err != nil {
			return fmt.Errorf("failed to attach SVI to VRF: %w", err)
		}
	}

	// Ensure SVI is up
	if sviLink.Attrs().Flags&net.FlagUp == 0 {
		if err := util.GetNetLinkOps().LinkSetUp(sviLink); err != nil {
			return fmt.Errorf("failed to set SVI up: %w", err)
		}
	}

	return nil
}

// deleteSVI removes the SVI for IP-VRF.
func (c *Controller) deleteSVI(networkName, vtepName string) error {
	// Need to get VID from network state
	c.mu.RLock()
	state := c.networkStates[networkName]
	c.mu.RUnlock()

	if state == nil || state.ipVRFVID == 0 {
		return nil
	}

	sviName := formatSVIName(vtepName, state.ipVRFVID)

	sviLink, err := util.GetNetLinkOps().LinkByName(sviName)
	if err != nil {
		if util.GetNetLinkOps().IsLinkNotFoundError(err) {
			return nil // Already gone
		}
		return fmt.Errorf("failed to get SVI: %w", err)
	}

	if err := util.GetNetLinkOps().LinkDelete(sviLink); err != nil {
		return fmt.Errorf("failed to delete SVI: %w", err)
	}

	klog.Infof("EVPN controller: deleted SVI %s", sviName)
	return nil
}

// --- Helpers ---

func cidrsEqual(a, b vtepv1.DualStackCIDRs) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// formatDeviceName creates a device name with prefix, handling length limits.
func formatDeviceName(prefix, base string) string {
	name := prefix + base
	if len(name) <= ifaceNameMaxLen {
		return name
	}
	// Truncate and add hash suffix for uniqueness
	h := fnv.New32a()
	h.Write([]byte(name))
	suffix := fmt.Sprintf("%x", h.Sum32())[:6]
	maxBase := max(0, ifaceNameMaxLen-len(prefix)-1-len(suffix))
	maxBase = min(maxBase, len(base))
	return fmt.Sprintf("%s%s-%s", prefix, base[:maxBase], suffix)
}

// formatSVIName creates an SVI name for a VTEP and VID.
func formatSVIName(vtepName string, vid int) string {
	return formatDeviceName(fmt.Sprintf("vlan%d-", vid), vtepName)
}

// generateMAC generates a deterministic MAC address from network name.
func generateMAC(networkName string) net.HardwareAddr {
	h := fnv.New32a()
	h.Write([]byte(networkName))
	hash := h.Sum32()
	return net.HardwareAddr{0xaa, 0xbb, 0xcc, byte(hash >> 16), byte(hash >> 8), byte(hash)}
}

// isExistsError checks if an error indicates the resource already exists.
func isExistsError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "exists")
}
