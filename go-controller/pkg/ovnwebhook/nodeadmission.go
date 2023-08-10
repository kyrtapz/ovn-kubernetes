package ovnwebhook

import (
	"context"
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"golang.org/x/exp/maps"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// TODO: Tests

type checkNodeAnnot func(removed, changed, added, nodeName string) error

// ovnkubeNodeAnnotations holds annotations allowed for ovnkube-node:<nodeName> users
var ovnkubeNodeAnnotations = map[string]checkNodeAnnot{
	util.OvnNodeHostAddresses: func(removed, changed, added, nodeName string) error {
		return nil
	},
	util.OvnNodeL3GatewayConfig: func(removed, changed, added, nodeName string) error {
		return nil
	},
	util.OvnNodeChassisID: func(removed, changed, added, nodeName string) error {
		if removed != "" {
			return fmt.Errorf("%s cannot be removed", util.OvnNodeChassisID)
		}
		if changed != "" {
			return fmt.Errorf("%s cannot be changed once set", util.OvnNodeChassisID)
		}
		return nil
	},
	util.OvnNodeManagementPortMacAddress: func(removed, changed, added, nodeName string) error {
		return nil
	},
	util.OvnNodeIfAddr: func(removed, changed, added, nodeName string) error {
		return nil
	},
	util.OvnNodeZoneName: func(removed, changed, added, nodeName string) error {
		klog.Info("removed: %q, changed: %q, added: %q, node: %q", removed, changed, added, nodeName)
		return nil
		// TODO: This won't work in multinode per zone
		//if added == "global" || added == nodeName || changed == nodeName {
		//	return nil
		//}
		//return fmt.Errorf("%s can only be set to global or node name", util.OvnNodeZoneName)
	},
	util.OvnNodeMigratedZoneName: func(removed, changed, added, nodeName string) error {
		// TODO
		return nil
	},
}

var ovnkubeNodeAnnotationsKeys = sets.New[string](maps.Keys(ovnkubeNodeAnnotations)...)

// TODO: This is not the correct approach if we want to have extra allowed
var allowedServiceAccounts = sets.New[string]("system:serviceaccount:ovn-kubernetes:ovnkube-master")

func mapDiff(old, new map[string]string) (removed, changed, added map[string]string) {
	removed = make(map[string]string)
	changed = make(map[string]string)
	added = make(map[string]string)

	for key, value := range old {
		v, found := new[key]
		if !found {
			removed[key] = value
			continue
		}
		if v != value {
			changed[key] = value
		}
	}
	for key, value := range new {
		if _, found := old[key]; !found {
			added[key] = value
		}
	}
	return
}

type NodeAdmission struct{}

var _ admission.CustomValidator = &NodeAdmission{}

func (p NodeAdmission) ValidateCreate(ctx context.Context, obj runtime.Object) (warnings admission.Warnings, err error) {
	// Ignores creation
	return nil, nil
}

func (p NodeAdmission) ValidateDelete(_ context.Context, _ runtime.Object) (warnings admission.Warnings, err error) {
	// Ignores deletion
	return nil, nil
}

func (p NodeAdmission) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (warnings admission.Warnings, err error) {
	oldNode := oldObj.(*corev1.Node)
	newNode := newObj.(*corev1.Node)

	removed, changed, added := mapDiff(oldNode.Annotations, newNode.Annotations)
	allChangedKeys := append(maps.Keys(removed), append(maps.Keys(changed), maps.Keys(added)...)...)

	if len(allChangedKeys) == 0 {
		// no annotation changes
		return nil, nil
	}

	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return nil, err
	}
	nodeName, isOVNKubeNode := ovnkubeNodeIdentity(req.UserInfo)
	if !isOVNKubeNode {
		// Do not allow modifying annotations that belong to ovnkube-node
		// NOTE: This doesn't protect other ovn-kubernetes annotations
		if !allowedServiceAccounts.Has(req.UserInfo.Username) && ovnkubeNodeAnnotationsKeys.HasAny(allChangedKeys...) {
			return nil, fmt.Errorf("user %q is not allowed to modify any of the following annotations on nodes: %v", req.UserInfo.Username, ovnkubeNodeAnnotationsKeys.UnsortedList())
		}
		return nil, nil
	}
	if newNode.Name != nodeName {
		return nil, fmt.Errorf("ovnkube-node on node: %s is not allowed to modify nodes %s annotations", nodeName, newNode.Name)
	}

	// ovnkube-node is not allowed to change annotations outside of it's scope
	if !ovnkubeNodeAnnotationsKeys.HasAll(allChangedKeys...) {
		return nil, fmt.Errorf("user %q is not allowed to set one the annotations on node: %s: %v", req.UserInfo.Username, newNode.Name, allChangedKeys)
	}

	for _, key := range allChangedKeys {
		if err := ovnkubeNodeAnnotations[key](removed[key], changed[key], added[key], nodeName); err != nil {
			return nil, fmt.Errorf("user %s is not allowed to set %s on node %s: %v", req.UserInfo.Username, key, newNode.Name, err)
		}
	}
	return nil, nil
}
