package ovnwebhook

import (
	"context"
	"fmt"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"golang.org/x/exp/maps"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// TODO: Tests

type checkPodAnnot func(removed, changed, added, nodeName string) error

var ovnkubePodAnnotations = map[string]checkPodAnnot{
	util.OvnPodAnnotationName: func(removed, changed, added, nodeName string) error {
		// TODO: Verify pod networks:
		// pass a client from the ctrl and check the nodes annotations every time?
		return nil
	},
	util.DPUConnectionDetailsAnnot: func(removed, changed, added, nodeName string) error {
		return nil
	},
	util.DPUConnetionStatusAnnot: func(removed, changed, added, nodeName string) error {
		return nil
	},
}

var ovnkubePodAnnotationsKeys = sets.New[string](maps.Keys(ovnkubePodAnnotations)...)

type PodAdmission struct{}

func (p PodAdmission) ValidateCreate(ctx context.Context, obj runtime.Object) (warnings admission.Warnings, err error) {
	// TODO: This is not effective if we created a webhook conf only for pod/status
	return p.checkPodChange(ctx, &corev1.Pod{}, obj)
}

func (p PodAdmission) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (warnings admission.Warnings, err error) {
	return p.checkPodChange(ctx, oldObj, newObj)
}

func (p PodAdmission) ValidateDelete(_ context.Context, _ runtime.Object) (warnings admission.Warnings, err error) {
	// Ignores deletion
	return nil, nil
}

var _ admission.CustomValidator = &PodAdmission{}

func (p PodAdmission) checkPodChange(ctx context.Context, oldObj, newObj runtime.Object) (warnings admission.Warnings, err error) {
	oldPod := oldObj.(*corev1.Pod)
	newPod := newObj.(*corev1.Pod)

	removed, changed, added := mapDiff(oldPod.Annotations, newPod.Annotations)
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
		if !allowedServiceAccounts.Has(req.UserInfo.Username) && ovnkubePodAnnotationsKeys.HasAny(allChangedKeys...) {
			return nil, fmt.Errorf("user %q is not allowed to modify any of the following annotations on pods: %v", req.UserInfo.Username, ovnkubePodAnnotationsKeys.UnsortedList())
		}
		return nil, nil
	}

	if oldPod.Spec.NodeName != nodeName {
		return nil, fmt.Errorf("ovnkube-node on node: %s is not allowed to modify pods %s annotations", nodeName, oldPod.Name)
	}
	if newPod.Spec.NodeName != nodeName {
		return nil, fmt.Errorf("ovnkube-node on node: %s is not allowed to modify pods %s annotations", nodeName, newPod.Name)
	}

	// ovnkube-node is not allowed to change annotations outside of it's scope
	if !ovnkubePodAnnotationsKeys.HasAll(allChangedKeys...) {
		return nil, fmt.Errorf("user %q is not allowed to set one the annotations on node: %s: %v", req.UserInfo.Username, newPod.Name, allChangedKeys)
	}

	for _, key := range allChangedKeys {
		if err := ovnkubePodAnnotations[key](removed[key], changed[key], added[key], nodeName); err != nil {
			return nil, fmt.Errorf("user %s is not allowed to set %s on node %s: %v", req.UserInfo.Username, key, newPod.Name, err)
		}
	}
	return nil, nil
}
