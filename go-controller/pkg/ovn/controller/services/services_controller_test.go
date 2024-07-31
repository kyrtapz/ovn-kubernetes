package services

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	globalconfig "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	kube_test "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"
	"golang.org/x/exp/maps"

	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	utilpointer "k8s.io/utils/pointer"
)

var (
	alwaysReady              = func() bool { return true }
	FakeGRs                  = "GR_1 GR_2"
	initialLsGroups []string = []string{types.ClusterLBGroupName, types.ClusterSwitchLBGroupName}
	initialLrGroups []string = []string{types.ClusterLBGroupName, types.ClusterRouterLBGroupName}

	outport int32       = int32(3456)
	tcp     v1.Protocol = v1.ProtocolTCP

	UDNNamespace       = "ns-udn"
	UDNNetworkName     = "tenant-red"
	UDNNetInfo         = getSampleUDNNetInfo(UDNNamespace)
	initialLsGroupsUDN = []string{
		UDNNetInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterLBGroupName),
		UDNNetInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterSwitchLBGroupName)}
	initialLrGroupsUDN = []string{
		UDNNetInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterLBGroupName),
		UDNNetInfo.GetNetworkScopedLoadBalancerGroupName(types.ClusterRouterLBGroupName)}
)

type serviceController struct {
	*Controller
	serviceStore       cache.Store
	endpointSliceStore cache.Store
	NADStore           cache.Store
	libovsdbCleanup    *libovsdbtest.Context
}

func newControllerWithDBSetup(dbSetup libovsdbtest.TestSetup) (*serviceController, error) {
	return newControllerWithDBSetupForNetwork(dbSetup, &util.DefaultNetInfo{}, false)
}

// func new() {
// 	var (
// 		app        *cli.App
// 		controller *Controller
// 		fakeClient *util.OVNClusterManagerClientset
// 	)

// 	start := func(objects ...runtime.Object) {
// 		config.OVNKubernetesFeature.EnableEgressFirewall = true
// 		config.OVNKubernetesFeature.EnableDNSNameResolver = true

// 		fakeClient = util.GetOVNClientset().GetOVNKubeControllerClientset()
// 		wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient)
// 		gomega.Expect(err).NotTo(gomega.HaveOccurred())
// 		controller, err = NewController(fakeClient, wf)
// 		gomega.Expect(err).NotTo(gomega.HaveOccurred())

// 		err = wf.Start()
// 		gomega.Expect(err).NotTo(gomega.HaveOccurred())

// 		err = controller.Start(context.Background(), 1)
// 		gomega.Expect(err).NotTo(gomega.HaveOccurred())
// 	}

// 	ginkgo.BeforeEach(func() {
// 		config.PrepareTestConfig()
// 		config.OVNKubernetesFeature.EnableInterconnect = true
// 		config.OVNKubernetesFeature.EnableMultiNetwork = true
// 		config.OVNKubernetesFeature.EnableNetworkSegmentation = true
// 		app = cli.NewApp()
// 		app.Name = "test"
// 		app.Flags = config.Flags
// 	})

// 	ginkgo.AfterEach(func() {
// 		if controller != nil {
// 			controller.Stop()
// 		}
// 	})

// }

func newControllerWithDBSetupForNetwork(dbSetup libovsdbtest.TestSetup, netInfo util.NetInfo, addNAD bool) (*serviceController, error) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(dbSetup, nil)
	if err != nil {
		return nil, err
	}
	// TODO client => informer factory => ovnk controller

	// client := fake.NewSimpleClientset() // TODO this cannot possibly support NADs, even if you have an mock informer that supports NADs
	// informerFactory := informers.NewSharedInformerFactory(client, 0)

	config.PrepareTestConfig()
	config.OVNKubernetesFeature.EnableInterconnect = true
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true
	config.OVNKubernetesFeature.EnableEgressFirewall = true
	config.OVNKubernetesFeature.EnableDNSNameResolver = true

	app := cli.NewApp()
	app.Name = "test"
	app.Flags = config.Flags

	client := util.GetOVNClientset().GetOVNKubeControllerClientset()
	// factoryMock := factoryMocks.NodeWatchFactory{}
	factoryMock, err := factory.NewOVNKubeControllerWatchFactory(client)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	factoryMock.Start()
	recorder := record.NewFakeRecorder(10)

	nbZoneFailed := false
	// Try to get the NBZone.  If there is an error, create NB_Global record.
	// Otherwise NewController() will return error since it
	// calls libovsdbutil.GetNBZone().
	_, err = libovsdbutil.GetNBZone(nbClient)
	if err != nil {
		nbZoneFailed = true
		err = createTestNBGlobal(nbClient, "global")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	// factoryMock := factoryMocks.NodeWatchFactory{}

	controller, err := NewController(client.KubeClient,
		nbClient,
		factoryMock.ServiceCoreInformer(), //,factoryMock.Core().V1().Services(),
		factoryMock.EndpointSliceCoreInformer(),
		factoryMock.NodeCoreInformer(),
		// &v1nadmocks.NetworkAttachmentDefinitionLister{},
		factoryMock.NADInformer().Lister(), // ***** PANICS HERE ****
		recorder,
		netInfo,
	)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())

	if nbZoneFailed {
		// Delete the NBGlobal row as this function created it.  Otherwise many tests would fail while
		// checking the expectedData in the NBDB.
		err = deleteTestNBGlobal(nbClient, "global")
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}

	controller.initTopLevelCache()
	controller.useLBGroups = true
	controller.useTemplates = true

	// if addNAD {
	// 	err = addSampleNAD(client)
	// 	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	// }
	// TODO: add namespace for UDN
	// TODO: service should be in that namespace

	return &serviceController{
		controller,
		factoryMock.ServiceCoreInformer().Informer().GetStore(), //,factoryMock.Core().V1().Services(),
		factoryMock.EndpointSliceInformer().GetStore(),
		factoryMock.NADInformer().Informer().GetStore(),
		cleanup,
	}, nil
}

func (c *serviceController) close() {
	c.libovsdbCleanup.Cleanup()
}

func getSampleUDNNetInfo(namespace string) util.NetInfo {
	netInfo, _ := util.NewNetInfo(&ovncnitypes.NetConf{
		// TODO What's the namespace here?
		Topology:   "layer3",
		NADName:    fmt.Sprintf("%s/nad1", namespace),
		MTU:        1400,
		Role:       "primary",
		Subnets:    "192.168.200.0/16",
		NetConf:    cnitypes.NetConf{Name: "tenant-red", Type: "ovn-k8s-cni-overlay"},
		JoinSubnet: "100.66.0.0/16",
	})
	return netInfo
}

func addSampleNAD(client *util.OVNKubeControllerClientset) error {
	_, err := client.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(
		context.TODO(),
		kube_test.GenerateNAD(UDNNetworkName, UDNNetworkName, UDNNamespace, types.Layer3Topology, "10.128.2.0/16/24", types.NetworkRolePrimary),
		metav1.CreateOptions{})
	return err
}

// TestSyncServices - an end-to-end test for the services controller.
func TestSyncServices(t *testing.T) {
	initialMaxLength := format.MaxLength
	temporarilyEnableGomegaMaxLengthFormat()
	t.Cleanup(func() {
		restoreGomegaMaxLengthFormat(initialMaxLength)
	})

	ns := "testns"
	serviceName := "foo"

	oldGateway := globalconfig.Gateway.Mode
	oldClusterSubnet := globalconfig.Default.ClusterSubnets
	globalconfig.Kubernetes.OVNEmptyLbEvents = true
	globalconfig.IPv4Mode = true
	defer func() {
		globalconfig.Kubernetes.OVNEmptyLbEvents = false
		globalconfig.IPv4Mode = false
		globalconfig.Gateway.Mode = oldGateway
		globalconfig.Default.ClusterSubnets = oldClusterSubnet
	}()
	_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
	_, cidr6, _ := net.ParseCIDR("fe00::/64")
	globalconfig.Default.ClusterSubnets = []globalconfig.CIDRNetworkEntry{{cidr4, 26}, {cidr6, 26}}
	var (
		nodeA = "node-a"
		nodeB = "node-b"
	)
	const (
		nodeAEndpointIP = "10.128.0.2"
		nodeBEndpointIP = "10.128.1.2"
		nodeAHostIP     = "10.0.0.1"
		nodeBHostIP     = "10.0.0.2"
	)
	firstNode := nodeConfig(nodeA, nodeAHostIP)
	secondNode := nodeConfig(nodeB, nodeBHostIP)
	defaultNodes := map[string]nodeInfo{
		nodeA: *firstNode,
		nodeB: *secondNode,
	}

	const nodePort = 8989

	tests := []struct {
		name                 string
		slice                *discovery.EndpointSlice
		service              *v1.Service
		initialDb            []libovsdbtest.TestData
		expectedDb           []libovsdbtest.TestData
		initialDbUDN         []libovsdbtest.TestData
		expectedDbUDN        []libovsdbtest.TestData
		gatewayMode          string
		nodeToDelete         *nodeInfo
		dbStateAfterDeleting []libovsdbtest.TestData
	}{

		{
			name: "create service from Single Stack Service without endpoints",
			slice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName + "ab23",
					Namespace: ns,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports:       []discovery.EndpointPort{},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints:   []discovery.Endpoint{},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
				Spec: v1.ServiceSpec{
					Type:       v1.ServiceTypeClusterIP,
					ClusterIP:  "192.168.1.1",
					ClusterIPs: []string{"192.168.1.1"},
					Selector:   map[string]string{"foo": "bar"},
					Ports: []v1.ServicePort{{
						Port:       80,
						Protocol:   v1.ProtocolTCP,
						TargetPort: intstr.FromInt(3456),
					}},
				},
			},
			initialDb: []libovsdbtest.TestData{
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
			},
			expectedDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "",
					},
					ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				nodeIPTemplate(firstNode),
				nodeIPTemplate(secondNode),
			},
			initialDbUDN: []libovsdbtest.TestData{
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitchForNetwork(nodeA, initialLsGroupsUDN, UDNNetInfo),
				nodeLogicalSwitchForNetwork(nodeB, initialLsGroupsUDN, UDNNetInfo),

				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				nodeLogicalRouterForNetwork(nodeA, initialLrGroupsUDN, UDNNetInfo),
				nodeLogicalRouterForNetwork(nodeB, initialLrGroupsUDN, UDNNetInfo),

				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterSwitchLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterRouterLBGroupName, UDNNetInfo),
			},
			expectedDbUDN: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "",
					},
					ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), UDNNetInfo.GetNetworkName()),
				},
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitchForNetwork(nodeA, initialLsGroupsUDN, UDNNetInfo),
				nodeLogicalSwitchForNetwork(nodeB, initialLsGroupsUDN, UDNNetInfo),

				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				nodeLogicalRouterForNetwork(nodeA, initialLrGroupsUDN, UDNNetInfo),
				nodeLogicalRouterForNetwork(nodeB, initialLrGroupsUDN, UDNNetInfo),

				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroupForNetwork(types.ClusterSwitchLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterRouterLBGroupName, UDNNetInfo),

				nodeIPTemplate(firstNode),
				nodeIPTemplate(secondNode),
			},
		},
		{
			name: "update service without endpoints",
			slice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName + "ab23",
					Namespace: ns,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports:       []discovery.EndpointPort{},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints:   []discovery.Endpoint{},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
				Spec: v1.ServiceSpec{
					Type:       v1.ServiceTypeClusterIP,
					ClusterIP:  "192.168.1.1",
					ClusterIPs: []string{"192.168.1.1"},
					Selector:   map[string]string{"foo": "bar"},
					Ports: []v1.ServicePort{{
						Port:       80,
						Protocol:   v1.ProtocolTCP,
						TargetPort: intstr.FromInt(3456),
					}},
				},
			},
			initialDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.0.1:6443": "",
					},
					ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitch("wrong-switch", []string{}, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeA, initialLrGroups, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeB, initialLrGroups, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter("node-c", []string{}, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
			},
			initialDbUDN: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.0.1:6443": "",
					},
					ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), UDNNetworkName),
				},
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitchForNetwork(nodeA, initialLsGroupsUDN, UDNNetInfo),
				nodeLogicalSwitchForNetwork(nodeB, initialLsGroupsUDN, UDNNetInfo),
				nodeLogicalSwitchForNetwork("wrong-switch", []string{}, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),

				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				// nodeLogicalRouter("node-c", []string{}, loadBalancerClusterWideTCPServiceName(ns, serviceName)), // TODO might not be cleaned up, thbough!
				nodeLogicalRouterForNetwork(nodeA, initialLrGroupsUDN, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouterForNetwork(nodeB, initialLrGroupsUDN, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouterForNetwork("node-c", []string{}, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),

				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterSwitchLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterRouterLBGroupName, UDNNetInfo),
			},
			expectedDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "",
					},
					ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitch("wrong-switch", []string{}),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				nodeLogicalRouter("node-c", []string{}),
				lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				nodeIPTemplate(firstNode),
				nodeIPTemplate(secondNode),
			},
			expectedDbUDN: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "",
					},
					ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), UDNNetworkName),
				},
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				// nodeLogicalSwitch("wrong-switch", []string{}),
				nodeLogicalSwitchForNetwork(nodeA, initialLsGroupsUDN, UDNNetInfo),
				nodeLogicalSwitchForNetwork(nodeB, initialLsGroupsUDN, UDNNetInfo),
				nodeLogicalSwitchForNetwork("wrong-switch", []string{}, UDNNetInfo),

				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				// nodeLogicalRouter("node-c", []string{}),
				nodeLogicalRouterForNetwork(nodeA, initialLrGroupsUDN, UDNNetInfo),
				nodeLogicalRouterForNetwork(nodeB, initialLrGroupsUDN, UDNNetInfo),
				nodeLogicalRouterForNetwork("node-c", []string{}, UDNNetInfo),

				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroupForNetwork(types.ClusterSwitchLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterRouterLBGroupName, UDNNetInfo),

				nodeIPTemplate(firstNode),
				nodeIPTemplate(secondNode),
			},
		},
		{
			name: "transition to endpoints, create nodeport",
			slice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName + "ab1",
					Namespace: ns,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports: []discovery.EndpointPort{
					{
						Protocol: &tcp,
						Port:     &outport,
					},
				},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Ready: utilpointer.Bool(true),
						},
						Addresses: []string{"10.128.0.2", "10.128.1.2"},
						NodeName:  &nodeA,
					},
				},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
				Spec: v1.ServiceSpec{
					Type:       v1.ServiceTypeClusterIP,
					ClusterIP:  "192.168.1.1",
					ClusterIPs: []string{"192.168.1.1"},
					Selector:   map[string]string{"foo": "bar"},
					Ports: []v1.ServicePort{{
						Port:       80,
						Protocol:   v1.ProtocolTCP,
						TargetPort: intstr.FromInt(3456),
						NodePort:   nodePort,
					}},
				},
			},
			initialDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.0.1:6443": "",
					},
					ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeLogicalSwitch(nodeA, initialLsGroups, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalSwitch(nodeB, initialLsGroups, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeA, initialLrGroups, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouter(nodeB, initialLrGroups, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
			},
			initialDbUDN: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.0.1:6443": "",
					},
					ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), UDNNetworkName),
				},
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitchForNetwork(nodeA, initialLsGroupsUDN, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalSwitchForNetwork(nodeB, initialLsGroupsUDN, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),

				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				nodeLogicalRouterForNetwork(nodeA, initialLrGroups, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				nodeLogicalRouterForNetwork(nodeB, initialLrGroups, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),

				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterSwitchLBGroupName, UDNNetInfo),
				lbGroupForNetwork(types.ClusterRouterLBGroupName, UDNNetInfo),
			},
			expectedDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "10.128.0.2:3456,10.128.1.2:3456",
					},
					ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeMergedTemplateLoadBalancer(nodePort, serviceName, ns, outport, nodeAEndpointIP, nodeBEndpointIP),
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterSwitchLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				lbGroup(types.ClusterRouterLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				nodeIPTemplate(firstNode),
				nodeIPTemplate(secondNode),
			},
			expectedDbUDN: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "10.128.0.2:3456,10.128.1.2:3456",
					},
					ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(ns, serviceName), UDNNetworkName),
				},
				nodeMergedTemplateLoadBalancerForNetwork(nodePort, serviceName, ns, outport, UDNNetworkName, nodeAEndpointIP, nodeBEndpointIP),

				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalSwitchForNetwork(nodeA, initialLsGroupsUDN, UDNNetInfo),
				nodeLogicalSwitchForNetwork(nodeB, initialLsGroupsUDN, UDNNetInfo),

				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				nodeLogicalRouterForNetwork(nodeA, initialLrGroups, UDNNetInfo),
				nodeLogicalRouterForNetwork(nodeB, initialLrGroups, UDNNetInfo),

				lbGroup(types.ClusterLBGroupName),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
				lbGroupForNetwork(types.ClusterLBGroupName, UDNNetInfo, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroupForNetwork(types.ClusterSwitchLBGroupName, UDNNetInfo, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				lbGroupForNetwork(types.ClusterRouterLBGroupName, UDNNetInfo, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),

				nodeIPTemplate(firstNode),
				nodeIPTemplate(secondNode),
			},
		},
		{
			name: "deleting a node should not leave stale load balancers",
			slice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName + "ab1",
					Namespace: ns,
					Labels:    map[string]string{discovery.LabelServiceName: serviceName},
				},
				Ports: []discovery.EndpointPort{
					{
						Protocol: &tcp,
						Port:     &outport,
					},
				},
				AddressType: discovery.AddressTypeIPv4,
				Endpoints: []discovery.Endpoint{
					{
						Conditions: discovery.EndpointConditions{
							Ready: utilpointer.Bool(true),
						},
						Addresses: []string{"10.128.0.2", "10.128.1.2"},
						NodeName:  &nodeA,
					},
				},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: serviceName, Namespace: ns},
				Spec: v1.ServiceSpec{
					Type:       v1.ServiceTypeClusterIP,
					ClusterIP:  "192.168.1.1",
					ClusterIPs: []string{"192.168.1.1"},
					Selector:   map[string]string{"foo": "bar"},
					Ports: []v1.ServicePort{{
						Port:       80,
						Protocol:   v1.ProtocolTCP,
						TargetPort: intstr.FromInt(3456),
						NodePort:   nodePort,
					}},
				},
			},
			initialDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.0.1:6443": "",
					},
					ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				lbGroup(types.ClusterLBGroupName,
					loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterSwitchLBGroupName),
				lbGroup(types.ClusterRouterLBGroupName),
			},
			expectedDb: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "10.128.0.2:3456,10.128.1.2:3456",
					},
					ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeMergedTemplateLoadBalancer(nodePort, serviceName, ns, outport, nodeAEndpointIP, nodeBEndpointIP),
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterSwitchLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				lbGroup(types.ClusterRouterLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				nodeIPTemplate(firstNode),
				nodeIPTemplate(secondNode),
			},
			nodeToDelete: nodeConfig(nodeA, nodeAHostIP),
			dbStateAfterDeleting: []libovsdbtest.TestData{
				&nbdb.LoadBalancer{
					UUID:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Name:     loadBalancerClusterWideTCPServiceName(ns, serviceName),
					Options:  servicesOptions(),
					Protocol: &nbdb.LoadBalancerProtocolTCP,
					Vips: map[string]string{
						"192.168.1.1:80": "10.128.0.2:3456,10.128.1.2:3456",
					},
					ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(ns, serviceName)),
				},
				nodeMergedTemplateLoadBalancer(nodePort, serviceName, ns, outport, nodeAEndpointIP, nodeBEndpointIP),
				nodeLogicalSwitch(nodeA, initialLsGroups),
				nodeLogicalSwitch(nodeB, initialLsGroups),
				nodeLogicalRouter(nodeA, initialLrGroups),
				nodeLogicalRouter(nodeB, initialLrGroups),
				lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(ns, serviceName)),
				lbGroup(types.ClusterSwitchLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				lbGroup(types.ClusterRouterLBGroupName, nodeMergedTemplateLoadBalancerName(ns, serviceName, v1.IPv4Protocol)),
				nodeIPTemplate(firstNode),
				nodeIPTemplate(secondNode),
			},
		},
	}

	for i, tt := range tests[:3] {
		for _, testUDN := range []bool{false, true} {
			udnString := ""
			if testUDN {
				udnString = "_UDN"
			}
			t.Run(fmt.Sprintf("%d_%s%s", i, tt.name, udnString), func(t *testing.T) {

				g := gomega.NewGomegaWithT(t)
				var netInfo util.NetInfo
				netInfo = &util.DefaultNetInfo{}
				initialDb := tt.initialDb
				expectedDb := tt.expectedDb
				if testUDN {
					netInfo = UDNNetInfo
					initialDb = tt.initialDbUDN
					expectedDb = tt.expectedDbUDN
				}

				if tt.gatewayMode != "" {
					globalconfig.Gateway.Mode = globalconfig.GatewayMode(tt.gatewayMode)
				} else {
					globalconfig.Gateway.Mode = globalconfig.GatewayModeShared
				}
				var controller *serviceController
				var err error

				controller, err = newControllerWithDBSetupForNetwork(libovsdbtest.TestSetup{NBData: initialDb}, netInfo, testUDN)

				if err != nil {
					t.Fatalf("Error creating controller: %v", err)
				}
				defer controller.close()

				// Add objects to the Store
				controller.endpointSliceStore.Add(tt.slice)
				controller.serviceStore.Add(tt.service)
				controller.nodeTracker.nodes = defaultNodes
				if testUDN {
					// Add mirrored slices
					controller.endpointSliceStore.Add(kube_test.MirrorEndpointSlice(tt.slice, UDNNetInfo.GetNetworkName()))
				}
				controller.RequestFullSync(controller.nodeTracker.getZoneNodes())

				err = controller.syncService(ns + "/" + serviceName)

				if err != nil {
					t.Fatalf("syncServices error: %v", err)
				}

				g.Expect(controller.nbClient).To(libovsdbtest.HaveData(expectedDb))

				if tt.nodeToDelete != nil {
					controller.nodeTracker.removeNode(tt.nodeToDelete.name)
					g.Expect(controller.syncService(namespacedServiceName(ns, serviceName))).To(gomega.Succeed())
					g.Expect(controller.nbClient).To(libovsdbtest.HaveData(tt.dbStateAfterDeleting))
				}
			})
			// // Run on UDN
			// t.Run(fmt.Sprintf("%d_%s_UDN", i, tt.name), func(t *testing.T) {
			// 	// tt.expectedDbUDN != nil

			// 	g := gomega.NewGomegaWithT(t)

			// 	if tt.gatewayMode != "" {
			// 		globalconfig.Gateway.Mode = globalconfig.GatewayMode(tt.gatewayMode)
			// 	} else {
			// 		globalconfig.Gateway.Mode = globalconfig.GatewayModeShared
			// 	}

			// 	controller, err := newControllerWithDBSetupForNetwork(libovsdbtest.TestSetup{NBData: tt.initialDbUDN}, UDNNetInfo, true)
			// 	if err != nil {
			// 		t.Fatalf("Error creating controller: %v", err)
			// 	}
			// 	defer controller.close()
			// 	// Add objects to the Store
			// 	controller.endpointSliceStore.Add(tt.slice)
			// 	// Add mirrored slices
			// 	controller.endpointSliceStore.Add(kube_test.MirrorEndpointSlice(tt.slice, UDNNetInfo.GetNetworkName()))

			// 	controller.serviceStore.Add(tt.service)

			// 	controller.nodeTracker.nodes = defaultNodes
			// 	controller.RequestFullSync(controller.nodeTracker.getZoneNodes())

			// 	err = controller.syncService(ns + "/" + serviceName)
			// 	if err != nil {
			// 		t.Fatalf("syncServices error: %v", err)
			// 	}

			// 	g.Expect(controller.nbClient).To(libovsdbtest.HaveData(tt.expectedDbUDN))

			// 	if tt.nodeToDelete != nil {
			// 		controller.nodeTracker.removeNode(tt.nodeToDelete.name)
			// 		g.Expect(controller.syncService(namespacedServiceName(ns, serviceName))).To(gomega.Succeed())
			// 		g.Expect(controller.nbClient).To(libovsdbtest.HaveData(tt.dbStateAfterDeleting))
			// 	}
			// })
		}

	}
}

func Test_ETPCluster_NodePort_Service_WithMultipleIPAddresses(t *testing.T) {
	g := gomega.NewGomegaWithT(t)
	globalconfig.IPv4Mode = true
	globalconfig.IPv6Mode = true
	_, cidr4, _ := net.ParseCIDR("10.128.0.0/16")
	_, cidr6, _ := net.ParseCIDR("fe00:0:0:0:5555::0/64")
	globalconfig.Default.ClusterSubnets = []globalconfig.CIDRNetworkEntry{{CIDR: cidr4, HostSubnetLength: 16}, {CIDR: cidr6, HostSubnetLength: 64}}

	nodeName := "node-a"
	nodeIPv4 := []net.IP{net.ParseIP("10.1.1.1"), net.ParseIP("10.2.2.2"), net.ParseIP("10.3.3.3")}
	nodeIPv6 := []net.IP{net.ParseIP("fd00:0:0:0:1::1"), net.ParseIP("fd00:0:0:0:2::2")}

	nodeA := nodeInfo{
		name:               nodeName,
		l3gatewayAddresses: []net.IP{nodeIPv4[0], nodeIPv6[0]},
		hostAddresses:      append(nodeIPv4, nodeIPv6...),
		gatewayRouterName:  nodeGWRouterName(nodeName), // TODO UDN-aware?
		switchName:         nodeSwitchName(nodeName),   // TODO UDN-aware?
		chassisID:          nodeName,
		zone:               types.OvnDefaultZone,
	}

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc-foo", Namespace: "namespace1"},
		Spec: v1.ServiceSpec{
			Type:                  v1.ServiceTypeNodePort,
			ClusterIP:             "192.168.1.1",
			ClusterIPs:            []string{"192.168.1.1", "fd00:0:0:0:7777::1"},
			IPFamilies:            []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
			Selector:              map[string]string{"foo": "bar"},
			ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeCluster,
			Ports: []v1.ServicePort{{
				Port:       80,
				Protocol:   v1.ProtocolTCP,
				TargetPort: intstr.FromInt(3456),
				NodePort:   30123,
			}},
		},
	}

	endPointSliceV4 := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name + "ipv4",
			Namespace: svc.Namespace,
			Labels:    map[string]string{discovery.LabelServiceName: svc.Name},
		},
		Ports:       []discovery.EndpointPort{{Protocol: &tcp, Port: &outport}},
		AddressType: discovery.AddressTypeIPv4,
		Endpoints:   kube_test.MakeReadyEndpointList(nodeName, "10.128.0.2", "10.128.1.2"),
	}

	endPointSliceV6 := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name + "ipv6",
			Namespace: svc.Namespace,
			Labels:    map[string]string{discovery.LabelServiceName: svc.Name},
		},
		Ports:       []discovery.EndpointPort{{Protocol: &tcp, Port: &outport}},
		AddressType: discovery.AddressTypeIPv6,
		Endpoints:   kube_test.MakeReadyEndpointList(nodeName, "fe00:0:0:0:5555::2", "fe00:0:0:0:5555::3"),
	}

	controller, err := newControllerWithDBSetup(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
		nodeLogicalSwitch(nodeA.name, initialLsGroups),
		nodeLogicalRouter(nodeA.name, initialLrGroups),

		lbGroup(types.ClusterLBGroupName),
		lbGroup(types.ClusterSwitchLBGroupName),
		lbGroup(types.ClusterRouterLBGroupName),
	}})
	g.Expect(err).ToNot(gomega.HaveOccurred())
	defer controller.close()

	controller.endpointSliceStore.Add(endPointSliceV4)
	controller.endpointSliceStore.Add(endPointSliceV6)
	controller.serviceStore.Add(svc)
	controller.nodeTracker.nodes = map[string]nodeInfo{nodeA.name: nodeA}

	controller.RequestFullSync(controller.nodeTracker.getZoneNodes())
	err = controller.syncService(svc.Namespace + "/" + svc.Name)
	g.Expect(err).ToNot(gomega.HaveOccurred())

	expectedDb := []libovsdbtest.TestData{
		&nbdb.LoadBalancer{
			UUID:     loadBalancerClusterWideTCPServiceName(svc.Namespace, svc.Name),
			Name:     loadBalancerClusterWideTCPServiceName(svc.Namespace, svc.Name),
			Options:  servicesOptions(),
			Protocol: &nbdb.LoadBalancerProtocolTCP,
			Vips: map[string]string{
				"192.168.1.1:80":        "10.128.0.2:3456,10.128.1.2:3456",
				"[fd00::7777:0:0:1]:80": "[fe00::5555:0:0:2]:3456,[fe00::5555:0:0:3]:3456",
			},
			ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(svc.Namespace, svc.Name)),
		},
		&nbdb.LoadBalancer{
			UUID:     "Service_namespace1/svc-foo_TCP_node_switch_template_IPv4_merged",
			Name:     "Service_namespace1/svc-foo_TCP_node_switch_template_IPv4_merged",
			Options:  templateServicesOptions(),
			Protocol: &nbdb.LoadBalancerProtocolTCP,
			Vips: map[string]string{
				"^NODEIP_IPv4_1:30123": "10.128.0.2:3456,10.128.1.2:3456",
				"^NODEIP_IPv4_2:30123": "10.128.0.2:3456,10.128.1.2:3456",
				"^NODEIP_IPv4_0:30123": "10.128.0.2:3456,10.128.1.2:3456",
			},
			ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(svc.Namespace, svc.Name)),
		},
		&nbdb.LoadBalancer{
			UUID:     "Service_namespace1/svc-foo_TCP_node_switch_template_IPv6_merged",
			Name:     "Service_namespace1/svc-foo_TCP_node_switch_template_IPv6_merged",
			Options:  templateServicesOptionsV6(),
			Protocol: &nbdb.LoadBalancerProtocolTCP,
			Vips: map[string]string{
				"^NODEIP_IPv6_1:30123": "[fe00::5555:0:0:2]:3456,[fe00::5555:0:0:3]:3456",
				"^NODEIP_IPv6_0:30123": "[fe00::5555:0:0:2]:3456,[fe00::5555:0:0:3]:3456",
			},
			ExternalIDs: loadBalancerExternalIDs(namespacedServiceName(svc.Namespace, svc.Name)),
		},
		nodeLogicalSwitch(nodeA.name, initialLsGroups),
		nodeLogicalRouter(nodeA.name, initialLrGroups),
		lbGroup(types.ClusterLBGroupName, loadBalancerClusterWideTCPServiceName(svc.Namespace, svc.Name)),
		lbGroup(types.ClusterSwitchLBGroupName,
			"Service_namespace1/svc-foo_TCP_node_switch_template_IPv4_merged",
			"Service_namespace1/svc-foo_TCP_node_switch_template_IPv6_merged"),
		lbGroup(types.ClusterRouterLBGroupName,
			"Service_namespace1/svc-foo_TCP_node_switch_template_IPv4_merged",
			"Service_namespace1/svc-foo_TCP_node_switch_template_IPv6_merged"),

		&nbdb.ChassisTemplateVar{
			UUID: nodeA.chassisID, Chassis: nodeA.chassisID,
			Variables: map[string]string{
				makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "0": nodeIPv4[0].String(),
				makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "1": nodeIPv4[1].String(),
				makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "2": nodeIPv4[2].String(),

				makeLBNodeIPTemplateNamePrefix(v1.IPv6Protocol) + "0": nodeIPv6[0].String(),
				makeLBNodeIPTemplateNamePrefix(v1.IPv6Protocol) + "1": nodeIPv6[1].String(),
			},
		},
	}

	g.Expect(controller.nbClient).To(libovsdbtest.HaveData(expectedDb))

}

func nodeLogicalSwitch(nodeName string, lbGroups []string, namespacedServiceNames ...string) *nbdb.LogicalSwitch {
	return nodeLogicalSwitchForNetwork(nodeName, lbGroups, &util.DefaultNetInfo{}, namespacedServiceNames...)
}

func nodeLogicalSwitchForNetwork(nodeName string, lbGroups []string, netInfo util.NetInfo, namespacedServiceNames ...string) *nbdb.LogicalSwitch {
	switchName := nodeSwitchNameForNetwork(nodeName, netInfo)
	ls := &nbdb.LogicalSwitch{
		UUID:              switchName,
		Name:              switchName,
		LoadBalancerGroup: lbGroups,
	}
	if !netInfo.IsDefault() {
		ls.ExternalIDs = getExternalIDsForNetwork(netInfo.GetNetworkName())
	}

	if len(namespacedServiceNames) > 0 {
		ls.LoadBalancer = namespacedServiceNames
	}
	return ls
}

func nodeLogicalRouter(nodeName string, lbGroups []string, namespacedServiceNames ...string) *nbdb.LogicalRouter {
	return nodeLogicalRouterForNetwork(nodeName, lbGroups, &util.DefaultNetInfo{}, namespacedServiceNames...)
}

func nodeLogicalRouterForNetwork(nodeName string, lbGroups []string, netInfo util.NetInfo, namespacedServiceNames ...string) *nbdb.LogicalRouter {
	routerName := nodeGWRouterNameForNetwork(nodeName, netInfo)
	lr := &nbdb.LogicalRouter{
		UUID:              routerName,
		Name:              routerName,
		LoadBalancerGroup: lbGroups,
	}
	if !netInfo.IsDefault() {
		lr.ExternalIDs = getExternalIDsForNetwork(netInfo.GetNetworkName())
	}

	if len(namespacedServiceNames) > 0 {
		lr.LoadBalancer = namespacedServiceNames
	}
	return lr
}

func nodeSwitchName(nodeName string) string {
	return nodeSwitchNameForNetwork(nodeName, &util.DefaultNetInfo{})
}

func nodeSwitchNameForNetwork(nodeName string, netInfo util.NetInfo) string {
	return netInfo.GetNetworkScopedSwitchName(fmt.Sprintf("switch-%s", nodeName))
}

func nodeGWRouterName(nodeName string) string {
	return nodeGWRouterNameForNetwork(nodeName, &util.DefaultNetInfo{})
}

func nodeGWRouterNameForNetwork(nodeName string, netInfo util.NetInfo) string {
	return netInfo.GetNetworkScopedGWRouterName(fmt.Sprintf("gr-%s", nodeName))
}

func lbGroup(name string, namespacedServiceNames ...string) *nbdb.LoadBalancerGroup {
	return lbGroupForNetwork(name, &util.DefaultNetInfo{}, namespacedServiceNames...)
}

func lbGroupForNetwork(name string, netInfo util.NetInfo, namespacedServiceNames ...string) *nbdb.LoadBalancerGroup {
	LBGroupName := netInfo.GetNetworkScopedLoadBalancerGroupName(name)
	lbg := &nbdb.LoadBalancerGroup{
		UUID: LBGroupName,
		Name: LBGroupName,
	}
	if len(namespacedServiceNames) > 0 {
		lbg.LoadBalancer = namespacedServiceNames
	}
	return lbg
}

func loadBalancerClusterWideTCPServiceName(ns string, serviceName string) string {
	return fmt.Sprintf("Service_%s_TCP_cluster", namespacedServiceName(ns, serviceName))
}

func namespacedServiceName(ns string, name string) string {
	return fmt.Sprintf("%s/%s", ns, name)
}

func nodeSwitchRouterLoadBalancerName(nodeName string, serviceNamespace string, serviceName string) string {
	return fmt.Sprintf(
		"Service_%s/%s_TCP_node_router+switch_%s",
		serviceNamespace,
		serviceName,
		nodeName)
}

func nodeSwitchTemplateLoadBalancerName(serviceNamespace string, serviceName string, addressFamily v1.IPFamily) string {
	return fmt.Sprintf(
		"Service_%s/%s_TCP_node_switch_template_%s",
		serviceNamespace,
		serviceName,
		addressFamily)
}

func nodeRouterTemplateLoadBalancerName(serviceNamespace string, serviceName string, addressFamily v1.IPFamily) string {
	return fmt.Sprintf(
		"Service_%s/%s_TCP_node_router_template_%s",
		serviceNamespace,
		serviceName,
		addressFamily)
}

func nodeMergedTemplateLoadBalancerName(serviceNamespace string, serviceName string, addressFamily v1.IPFamily) string {
	return fmt.Sprintf(
		"Service_%s/%s_TCP_node_switch_template_%s_merged",
		serviceNamespace,
		serviceName,
		addressFamily)
}

func servicesOptions() map[string]string {
	return map[string]string{
		"event":              "false",
		"reject":             "true",
		"skip_snat":          "false",
		"neighbor_responder": "none",
		"hairpin_snat_ip":    "169.254.169.5 fd69::5",
	}
}

func servicesOptionsWithAffinityTimeout() map[string]string {
	options := servicesOptions()
	options["affinity_timeout"] = "10800"
	return options
}

func templateServicesOptions() map[string]string {
	// Template LBs need "options:template=true" and "options:address-family" set.
	opts := servicesOptions()
	opts["template"] = "true"
	opts["address-family"] = "ipv4"
	return opts
}

func templateServicesOptionsV6() map[string]string {
	// Template LBs need "options:template=true" and "options:address-family" set.
	opts := servicesOptions()
	opts["template"] = "true"
	opts["address-family"] = "ipv6"
	return opts
}

func tcpGatewayRouterExternalIDs() map[string]string {
	return map[string]string{
		"TCP_lb_gateway_router": "",
	}
}
func getExternalIDsForNetwork(network string) map[string]string {
	role := types.NetworkRoleDefault
	if network != types.DefaultNetworkName {
		role = types.NetworkRolePrimary
	}
	return map[string]string{
		types.NetworkRoleExternalID: role,
		types.NetworkExternalID:     network,
	}
}

func loadBalancerExternalIDs(namespacedServiceName string) map[string]string {
	return loadBalancerExternalIDsForNetwork(namespacedServiceName, types.DefaultNetworkName)
}

func loadBalancerExternalIDsForNetwork(namespacedServiceName string, network string) map[string]string {
	externalIDs := map[string]string{
		types.LoadBalancerKindExternalID:  "Service",
		types.LoadBalancerOwnerExternalID: namespacedServiceName,
	}
	maps.Copy(externalIDs, getExternalIDsForNetwork(network))
	return externalIDs

}

func nodeIPTemplate(node *nodeInfo) *nbdb.ChassisTemplateVar {
	return &nbdb.ChassisTemplateVar{
		UUID:    node.chassisID,
		Chassis: node.chassisID,
		Variables: map[string]string{
			makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "0": node.hostAddresses[0].String(),
		},
	}
}

func nodeMergedTemplateLoadBalancer(nodePort int32, serviceName string, serviceNamespace string, outputPort int32, endpointIPs ...string) *nbdb.LoadBalancer {
	return nodeMergedTemplateLoadBalancerForNetwork(nodePort, serviceName, serviceNamespace, outputPort, types.DefaultNetworkName, endpointIPs...)
}

func nodeMergedTemplateLoadBalancerForNetwork(nodePort int32, serviceName string, serviceNamespace string, outputPort int32, networkName string, endpointIPs ...string) *nbdb.LoadBalancer {
	nodeTemplateIP := makeTemplate(makeLBNodeIPTemplateNamePrefix(v1.IPv4Protocol) + "0")
	return &nbdb.LoadBalancer{
		UUID:     nodeMergedTemplateLoadBalancerName(serviceNamespace, serviceName, v1.IPv4Protocol),
		Name:     nodeMergedTemplateLoadBalancerName(serviceNamespace, serviceName, v1.IPv4Protocol),
		Options:  templateServicesOptions(),
		Protocol: &nbdb.LoadBalancerProtocolTCP,
		Vips: map[string]string{
			endpoint(refTemplate(nodeTemplateIP.Name), nodePort): computeEndpoints(outputPort, endpointIPs...),
		},
		ExternalIDs: loadBalancerExternalIDsForNetwork(namespacedServiceName(serviceNamespace, serviceName), networkName),
	}
}

func refTemplate(template string) string {
	return "^" + template
}

func makeTarget(serviceName, serviceNamespace string, proto v1.Protocol, outputPort int32, scope string, addressFamily v1.IPFamily) string {
	return makeTemplateName(
		fmt.Sprintf("Service_%s/%s_%s_%d_%s_%s",
			serviceNamespace, serviceName,
			proto, outputPort, scope, addressFamily)) /// TODO add netInfo
}

func computeEndpoints(outputPort int32, ips ...string) string {
	var endpoints []string
	for _, ip := range ips {
		endpoints = append(endpoints, endpoint(ip, outputPort))
	}
	return strings.Join(endpoints, ",")
}

func endpoint(ip string, port int32) string {
	return fmt.Sprintf("%s:%d", ip, port)
}

func nodeConfig(nodeName string, nodeIP string) *nodeInfo {
	return &nodeInfo{
		name:               nodeName,
		l3gatewayAddresses: []net.IP{net.ParseIP(nodeIP)},
		hostAddresses:      []net.IP{net.ParseIP(nodeIP)},
		gatewayRouterName:  nodeGWRouterName(nodeName),
		switchName:         nodeSwitchName(nodeName),
		chassisID:          nodeName,
		zone:               types.OvnDefaultZone,
	}
}

func temporarilyEnableGomegaMaxLengthFormat() {
	format.MaxLength = 0
}

func restoreGomegaMaxLengthFormat(originalLength int) {
	format.MaxLength = originalLength
}

func createTestNBGlobal(nbClient libovsdbclient.Client, zone string) error {
	nbGlobal := &nbdb.NBGlobal{Name: zone}
	ops, err := nbClient.Create(nbGlobal)
	if err != nil {
		return err
	}

	_, err = nbClient.Transact(context.Background(), ops...)
	if err != nil {
		return err
	}

	return nil
}

func deleteTestNBGlobal(nbClient libovsdbclient.Client, zone string) error {
	p := func(nbGlobal *nbdb.NBGlobal) bool {
		return true
	}

	ops, err := nbClient.WhereCache(p).Delete()
	if err != nil {
		return err
	}

	_, err = nbClient.Transact(context.Background(), ops...)
	if err != nil {
		return err
	}

	return nil
}
