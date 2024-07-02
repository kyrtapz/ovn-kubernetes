package node

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

// SecondaryNodeNetworkController structure is the object which holds the controls for starting
// and reacting upon the watched resources (e.g. pods, endpoints) for secondary network
type SecondaryNodeNetworkController struct {
	BaseNodeNetworkController

	gatewayManager NodeSecondaryGatewayManager

	// pod events factory handler
	podHandler *factory.Handler

	networkID int

	// responsible for programing gateway elements for this network
	gateway *UserDefinedNetworkGateway
}

// NewSecondaryNodeNetworkController creates a new OVN controller for creating logical network
// infrastructure and policy for default l3 network
func NewSecondaryNodeNetworkController(cnnci *CommonNodeNetworkControllerInfo, netInfo util.NetInfo, gwManager NodeSecondaryGatewayManager) *SecondaryNodeNetworkController {
	nc := &SecondaryNodeNetworkController{
		BaseNodeNetworkController: BaseNodeNetworkController{
			CommonNodeNetworkControllerInfo: *cnnci,
			NetInfo:                         netInfo,
			stopChan:                        make(chan struct{}),
			wg:                              &sync.WaitGroup{},
		},
		gatewayManager: gwManager,
	}
	nc.initRetryFrameworkForNode()
	return nc
}

// Start starts the default controller; handles all events and creates all needed logical entities
func (nc *SecondaryNodeNetworkController) Start(ctx context.Context) error {
	klog.Infof("Start secondary node network controller of network %s", nc.GetNetworkName())

	// enable adding ovs ports for dpu pods in both primary and secondary user defined networks
	if (config.OVNKubernetesFeature.EnableMultiNetwork || util.IsNetworkSegmentationSupportEnabled()) && config.OvnKubeNode.Mode == types.NodeModeDPU {
		handler, err := nc.watchPodsDPU()
		if err != nil {
			return err
		}
		nc.podHandler = handler
	}
	if util.IsNetworkSegmentationSupportEnabled() && nc.IsPrimaryNetwork() {
		node, err := nc.watchFactory.GetNode(nc.name)
		if err != nil {
			return err
		}

		nc.gateway = NewUserDefinedNetworkGateway(nc.NetInfo, nc.networkID, node)
		if err := nc.gateway.AddNetwork(); err != nil {
			return fmt.Errorf("failed to add network to node gateway for network %s at node %s: %w",
				nc.GetNetworkName(), nc.name, err)
		}
	}

	// Generate a per network conntrack mark to be used for egress traffic.
	masqCTMark := ctMarkUDNBase + uint(nc.networkID)

	if err := nc.gatewayManager.AddNetwork(nc.NetInfo, masqCTMark); err != nil {
		return fmt.Errorf("failed to add network to node gateway for network '%s': %w", nc.GetNetworkName(), err)
	}

	err := nc.WatchEndpointSlices() // TODO call it also from secondary node network controller
	if err != nil {
		return fmt.Errorf("failed to watch endpointSlices: %w", err)
	}

	// TODO for now using config.Default.Zone;
	// TODO sbZone should not be recomputed, but passed from default node network controller

	// If interconnect is disabled OR interconnect is running in single-zone-mode,
	// the ovnkube-master is responsible for patching ICNI managed namespaces with
	// "k8s.ovn.org/external-gw-pod-ips". In that case, we need ovnkube-node to flush
	// conntrack on every node. In multi-zone-interconnect case, we will handle the flushing
	// directly on the ovnkube-controller code to avoid an extra namespace annotation
	if !config.OVNKubernetesFeature.EnableInterconnect || config.Default.Zone == types.OvnDefaultZone {
		err := nc.WatchNamespaces()
		if err != nil {
			return fmt.Errorf("failed to watch namespaces: %w", err)
		}
		// every minute cleanup stale conntrack entries if any
		go wait.Until(func() {
			nc.checkAndDeleteStaleConntrackEntries()
		}, time.Minute*1, nc.stopChan)
	}
	err = nc.WatchEndpointSlices() // TODO call it also from secondary node network controller
	if err != nil {
		return fmt.Errorf("failed to watch endpointSlices: %w", err)
	}

	return nil
}

// Stop gracefully stops the controller
func (nc *SecondaryNodeNetworkController) Stop() {
	klog.Infof("Stop secondary node network controller of network %s", nc.GetNetworkName())
	close(nc.stopChan)
	nc.wg.Wait()

	if nc.podHandler != nil {
		nc.watchFactory.RemovePodHandler(nc.podHandler)
	}

	//TODO (dceara): does this need to go in Cleanup?
	if err := nc.gatewayManager.DelNetwork(nc); err != nil {
		// TODO (dceara): handle error
		klog.Errorf("Failed to delete network from node gateway for network '%s'", nc.GetNetworkName())
	}
}

// Cleanup cleans up node entities for the given secondary network
func (nc *SecondaryNodeNetworkController) Cleanup() error {
	if nc.gateway != nil {
		return nc.gateway.DelNetwork()
	}
	return nil
}

// TODO(dceara): identical to BaseSecondaryNetworkController.ensureNetworkID()
func (nc *SecondaryNodeNetworkController) ensureNetworkID() error {
	if nc.networkID != 0 {
		return nil
	}
	nodes, err := nc.watchFactory.GetNodes()
	if err != nil {
		return fmt.Errorf("failed to get nodes: %v", err)
	}
	networkID := util.InvalidNetworkID
	for _, node := range nodes {
		networkID, err = util.ParseNetworkIDAnnotation(node, nc.GetNetworkName())
		if err != nil {
			//TODO Warning
			continue
		}
	}
	if networkID == util.InvalidNetworkID {
		return fmt.Errorf("missing network id for network '%s'", nc.GetNetworkName())
	}
	nc.networkID = networkID
	return nil
}
