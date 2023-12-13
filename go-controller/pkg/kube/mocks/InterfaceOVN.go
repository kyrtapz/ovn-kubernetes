// Code generated by mockery v2.15.0. DO NOT EDIT.

package mocks

import (
	egressfirewallv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	apicorev1 "k8s.io/api/core/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mock "github.com/stretchr/testify/mock"

	v1 "github.com/openshift/api/cloudnetwork/v1"
)

// InterfaceOVN is an autogenerated mock type for the InterfaceOVN type
type InterfaceOVN struct {
	mock.Mock
}

// CreateCloudPrivateIPConfig provides a mock function with given fields: cloudPrivateIPConfig
func (_m *InterfaceOVN) CreateCloudPrivateIPConfig(cloudPrivateIPConfig *v1.CloudPrivateIPConfig) (*v1.CloudPrivateIPConfig, error) {
	ret := _m.Called(cloudPrivateIPConfig)

	var r0 *v1.CloudPrivateIPConfig
	if rf, ok := ret.Get(0).(func(*v1.CloudPrivateIPConfig) *v1.CloudPrivateIPConfig); ok {
		r0 = rf(cloudPrivateIPConfig)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*v1.CloudPrivateIPConfig)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(*v1.CloudPrivateIPConfig) error); ok {
		r1 = rf(cloudPrivateIPConfig)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// DeleteCloudPrivateIPConfig provides a mock function with given fields: name
func (_m *InterfaceOVN) DeleteCloudPrivateIPConfig(name string) error {
	ret := _m.Called(name)

	var r0 error
	if rf, ok := ret.Get(0).(func(string) error); ok {
		r0 = rf(name)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// Events provides a mock function with given fields:
func (_m *InterfaceOVN) Events() corev1.EventInterface {
	ret := _m.Called()

	var r0 corev1.EventInterface
	if rf, ok := ret.Get(0).(func() corev1.EventInterface); ok {
		r0 = rf()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(corev1.EventInterface)
		}
	}

	return r0
}

// GetAnnotationsOnPod provides a mock function with given fields: namespace, name
func (_m *InterfaceOVN) GetAnnotationsOnPod(namespace string, name string) (map[string]string, error) {
	ret := _m.Called(namespace, name)

	var r0 map[string]string
	if rf, ok := ret.Get(0).(func(string, string) map[string]string); ok {
		r0 = rf(namespace, name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(map[string]string)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string, string) error); ok {
		r1 = rf(namespace, name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetEgressFirewalls provides a mock function with given fields:
func (_m *InterfaceOVN) GetEgressFirewalls() (*egressfirewallv1.EgressFirewallList, error) {
	ret := _m.Called()

	var r0 *egressfirewallv1.EgressFirewallList
	if rf, ok := ret.Get(0).(func() *egressfirewallv1.EgressFirewallList); ok {
		r0 = rf()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*egressfirewallv1.EgressFirewallList)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func() error); ok {
		r1 = rf()
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetEgressIP provides a mock function with given fields: name
func (_m *InterfaceOVN) GetEgressIP(name string) (*egressipv1.EgressIP, error) {
	ret := _m.Called(name)

	var r0 *egressipv1.EgressIP
	if rf, ok := ret.Get(0).(func(string) *egressipv1.EgressIP); ok {
		r0 = rf(name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*egressipv1.EgressIP)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string) error); ok {
		r1 = rf(name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetEgressIPs provides a mock function with given fields:
func (_m *InterfaceOVN) GetEgressIPs() (*egressipv1.EgressIPList, error) {
	ret := _m.Called()

	var r0 *egressipv1.EgressIPList
	if rf, ok := ret.Get(0).(func() *egressipv1.EgressIPList); ok {
		r0 = rf()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*egressipv1.EgressIPList)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func() error); ok {
		r1 = rf()
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetNamespaces provides a mock function with given fields: labelSelector
func (_m *InterfaceOVN) GetNamespaces(labelSelector metav1.LabelSelector) (*apicorev1.NamespaceList, error) {
	ret := _m.Called(labelSelector)

	var r0 *apicorev1.NamespaceList
	if rf, ok := ret.Get(0).(func(metav1.LabelSelector) *apicorev1.NamespaceList); ok {
		r0 = rf(labelSelector)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*apicorev1.NamespaceList)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(metav1.LabelSelector) error); ok {
		r1 = rf(labelSelector)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetNode provides a mock function with given fields: name
func (_m *InterfaceOVN) GetNode(name string) (*apicorev1.Node, error) {
	ret := _m.Called(name)

	var r0 *apicorev1.Node
	if rf, ok := ret.Get(0).(func(string) *apicorev1.Node); ok {
		r0 = rf(name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*apicorev1.Node)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string) error); ok {
		r1 = rf(name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetNodes provides a mock function with given fields:
func (_m *InterfaceOVN) GetNodes() (*apicorev1.NodeList, error) {
	ret := _m.Called()

	var r0 *apicorev1.NodeList
	if rf, ok := ret.Get(0).(func() *apicorev1.NodeList); ok {
		r0 = rf()
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*apicorev1.NodeList)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func() error); ok {
		r1 = rf()
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetPod provides a mock function with given fields: namespace, name
func (_m *InterfaceOVN) GetPod(namespace string, name string) (*apicorev1.Pod, error) {
	ret := _m.Called(namespace, name)

	var r0 *apicorev1.Pod
	if rf, ok := ret.Get(0).(func(string, string) *apicorev1.Pod); ok {
		r0 = rf(namespace, name)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*apicorev1.Pod)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string, string) error); ok {
		r1 = rf(namespace, name)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetPods provides a mock function with given fields: namespace, labelSelector
func (_m *InterfaceOVN) GetPods(namespace string, labelSelector metav1.LabelSelector) (*apicorev1.PodList, error) {
	ret := _m.Called(namespace, labelSelector)

	var r0 *apicorev1.PodList
	if rf, ok := ret.Get(0).(func(string, metav1.LabelSelector) *apicorev1.PodList); ok {
		r0 = rf(namespace, labelSelector)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*apicorev1.PodList)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string, metav1.LabelSelector) error); ok {
		r1 = rf(namespace, labelSelector)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// PatchEgressIP provides a mock function with given fields: name, patchData
func (_m *InterfaceOVN) PatchEgressIP(name string, patchData []byte) error {
	ret := _m.Called(name, patchData)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, []byte) error); ok {
		r0 = rf(name, patchData)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// PatchNode provides a mock function with given fields: old, new
func (_m *InterfaceOVN) PatchNode(old *apicorev1.Node, new *apicorev1.Node) error {
	ret := _m.Called(old, new)

	var r0 error
	if rf, ok := ret.Get(0).(func(*apicorev1.Node, *apicorev1.Node) error); ok {
		r0 = rf(old, new)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// RemoveTaintFromNode provides a mock function with given fields: nodeName, taint
func (_m *InterfaceOVN) RemoveTaintFromNode(nodeName string, taint *apicorev1.Taint) error {
	ret := _m.Called(nodeName, taint)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *apicorev1.Taint) error); ok {
		r0 = rf(nodeName, taint)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetAnnotationsOnNamespace provides a mock function with given fields: namespaceName, annotations
func (_m *InterfaceOVN) SetAnnotationsOnNamespace(namespaceName string, annotations map[string]interface{}) error {
	ret := _m.Called(namespaceName, annotations)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, map[string]interface{}) error); ok {
		r0 = rf(namespaceName, annotations)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetAnnotationsOnNode provides a mock function with given fields: nodeName, annotations
func (_m *InterfaceOVN) SetAnnotationsOnNode(nodeName string, annotations map[string]interface{}) error {
	ret := _m.Called(nodeName, annotations)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, map[string]interface{}) error); ok {
		r0 = rf(nodeName, annotations)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetAnnotationsOnPod provides a mock function with given fields: namespace, podName, annotations
func (_m *InterfaceOVN) SetAnnotationsOnPod(namespace string, podName string, annotations map[string]interface{}) error {
	ret := _m.Called(namespace, podName, annotations)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, string, map[string]interface{}) error); ok {
		r0 = rf(namespace, podName, annotations)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetAnnotationsOnService provides a mock function with given fields: namespace, serviceName, annotations
func (_m *InterfaceOVN) SetAnnotationsOnService(namespace string, serviceName string, annotations map[string]interface{}) error {
	ret := _m.Called(namespace, serviceName, annotations)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, string, map[string]interface{}) error); ok {
		r0 = rf(namespace, serviceName, annotations)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetLabelsOnNode provides a mock function with given fields: nodeName, labels
func (_m *InterfaceOVN) SetLabelsOnNode(nodeName string, labels map[string]interface{}) error {
	ret := _m.Called(nodeName, labels)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, map[string]interface{}) error); ok {
		r0 = rf(nodeName, labels)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// SetTaintOnNode provides a mock function with given fields: nodeName, taint
func (_m *InterfaceOVN) SetTaintOnNode(nodeName string, taint *apicorev1.Taint) error {
	ret := _m.Called(nodeName, taint)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *apicorev1.Taint) error); ok {
		r0 = rf(nodeName, taint)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdateCloudPrivateIPConfig provides a mock function with given fields: cloudPrivateIPConfig
func (_m *InterfaceOVN) UpdateCloudPrivateIPConfig(cloudPrivateIPConfig *v1.CloudPrivateIPConfig) (*v1.CloudPrivateIPConfig, error) {
	ret := _m.Called(cloudPrivateIPConfig)

	var r0 *v1.CloudPrivateIPConfig
	if rf, ok := ret.Get(0).(func(*v1.CloudPrivateIPConfig) *v1.CloudPrivateIPConfig); ok {
		r0 = rf(cloudPrivateIPConfig)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*v1.CloudPrivateIPConfig)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(*v1.CloudPrivateIPConfig) error); ok {
		r1 = rf(cloudPrivateIPConfig)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// UpdateEgressFirewall provides a mock function with given fields: egressfirewall
func (_m *InterfaceOVN) UpdateEgressFirewall(egressfirewall *egressfirewallv1.EgressFirewall) error {
	ret := _m.Called(egressfirewall)

	var r0 error
	if rf, ok := ret.Get(0).(func(*egressfirewallv1.EgressFirewall) error); ok {
		r0 = rf(egressfirewall)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdateEgressIP provides a mock function with given fields: eIP
func (_m *InterfaceOVN) UpdateEgressIP(eIP *egressipv1.EgressIP) error {
	ret := _m.Called(eIP)

	var r0 error
	if rf, ok := ret.Get(0).(func(*egressipv1.EgressIP) error); ok {
		r0 = rf(eIP)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdateEgressServiceStatus provides a mock function with given fields: namespace, name, host
func (_m *InterfaceOVN) UpdateEgressServiceStatus(namespace string, name string, host string) error {
	ret := _m.Called(namespace, name, host)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, string, string) error); ok {
		r0 = rf(namespace, name, host)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdateNodeStatus provides a mock function with given fields: node
func (_m *InterfaceOVN) UpdateNodeStatus(node *apicorev1.Node) error {
	ret := _m.Called(node)

	var r0 error
	if rf, ok := ret.Get(0).(func(*apicorev1.Node) error); ok {
		r0 = rf(node)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// UpdatePod provides a mock function with given fields: pod
func (_m *InterfaceOVN) UpdatePodStatus(pod *apicorev1.Pod) error {
	ret := _m.Called(pod)

	var r0 error
	if rf, ok := ret.Get(0).(func(*apicorev1.Pod) error); ok {
		r0 = rf(pod)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

type mockConstructorTestingTNewInterfaceOVN interface {
	mock.TestingT
	Cleanup(func())
}

// NewInterfaceOVN creates a new instance of InterfaceOVN. It also registers a testing interface on the mock and a cleanup function to assert the mocks expectations.
func NewInterfaceOVN(t mockConstructorTestingTNewInterfaceOVN) *InterfaceOVN {
	mock := &InterfaceOVN{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}