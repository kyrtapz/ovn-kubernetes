// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controllermanager

import (
	"sync"

	corev1 "k8s.io/api/core/v1"
	knet "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

const ovnStatelessNetPolAnnotation = "k8s.ovn.org/acl-stateless"

// NamespacedResourceControllers holds per-network controllers for all
// namespace-scoped resource types dispatched by NamespacedResourceDispatcher.
type NamespacedResourceControllers struct {
	Pod       controller.Reconciler
	Namespace controller.Reconciler
	NetPol    controller.Reconciler
}

// NamespacedResourceDispatcher registers a single event handler per resource
// type (pods, namespaces, network policies) on the shared informers and
// dispatches events to per-network workqueues based on namespace ownership.
//
// This replaces per-network handler registrations through the watch/retry
// framework, reducing processorListener goroutines from O(networks) to O(1)
// per resource type and eliminating fan-out where every event wakes all
// network handlers.
type NamespacedResourceDispatcher struct {
	mu          sync.RWMutex
	controllers map[string]*NamespacedResourceControllers

	// pendingDeletes stores pod objects from delete events so the reconciler
	// can access annotations after the pod is removed from the lister.
	pendingDeletes sync.Map

	podHandler    cache.ResourceEventHandlerRegistration
	nsHandler     cache.ResourceEventHandlerRegistration
	netPolHandler cache.ResourceEventHandlerRegistration
}

// NewNamespacedResourceDispatcher creates a new dispatcher.
func NewNamespacedResourceDispatcher() *NamespacedResourceDispatcher {
	return &NamespacedResourceDispatcher{
		controllers: make(map[string]*NamespacedResourceControllers),
	}
}

// Start registers event handlers on the pod, namespace, and network policy
// informers.
func (d *NamespacedResourceDispatcher) Start(wf *factory.WatchFactory) error {
	var err error

	d.podHandler, err = wf.PodCoreInformer().Informer().AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    d.onPodAdd,
			UpdateFunc: d.onPodUpdate,
			DeleteFunc: d.onPodDelete,
		}))
	if err != nil {
		return err
	}

	d.nsHandler, err = wf.NamespaceCoreInformer().Informer().AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    d.onNamespaceAdd,
			UpdateFunc: d.onNamespaceUpdate,
			DeleteFunc: d.onNamespaceDelete,
		}))
	if err != nil {
		d.removeHandler(wf.PodCoreInformer().Informer(), &d.podHandler)
		return err
	}

	klog.Infof("Namespaced resource dispatcher started (pods, namespaces)")
	return nil
}

// StartNetworkPolicyHandler registers the network policy event handler. Called
// separately because the network policy informer may not be initialized in all
// configurations.
func (d *NamespacedResourceDispatcher) StartNetworkPolicyHandler(informer cache.SharedIndexInformer) error {
	var err error
	d.netPolHandler, err = informer.AddEventHandler(
		factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
			AddFunc:    d.onNetPolAdd,
			UpdateFunc: d.onNetPolUpdate,
			DeleteFunc: d.onNetPolDelete,
		}))
	if err != nil {
		return err
	}
	klog.Infof("Namespaced resource dispatcher: network policy handler started")
	return nil
}

func (d *NamespacedResourceDispatcher) removeHandler(informer cache.SharedIndexInformer, handler *cache.ResourceEventHandlerRegistration) {
	if *handler != nil {
		if err := informer.RemoveEventHandler(*handler); err != nil {
			klog.Errorf("Failed to remove dispatcher handler: %v", err)
		}
		*handler = nil
	}
}

// Stop removes all event handlers.
func (d *NamespacedResourceDispatcher) Stop(wf *factory.WatchFactory) {
	d.removeHandler(wf.PodCoreInformer().Informer(), &d.podHandler)
	d.removeHandler(wf.NamespaceCoreInformer().Informer(), &d.nsHandler)
}

// AddControllers registers per-network controllers for the given namespaces.
func (d *NamespacedResourceDispatcher) AddControllers(namespaces []string, ctrls *NamespacedResourceControllers) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ns := range namespaces {
		d.controllers[ns] = ctrls
	}
}

// RemoveControllers removes controllers for the given namespaces.
func (d *NamespacedResourceDispatcher) RemoveControllers(namespaces []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ns := range namespaces {
		delete(d.controllers, ns)
	}
}

func (d *NamespacedResourceDispatcher) getControllers(namespace string) *NamespacedResourceControllers {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.controllers[namespace]
}

// GetPendingDelete retrieves a pending-delete pod entry without removing it.
// Returns nil if no entry exists for the given key. The entry is kept so that
// retries after a failed delete can still access the pod object. Call
// DeletePendingDelete after the delete succeeds.
func (d *NamespacedResourceDispatcher) GetPendingDelete(key string) *corev1.Pod {
	v, ok := d.pendingDeletes.Load(key)
	if !ok {
		return nil
	}
	return v.(*corev1.Pod)
}

// DeletePendingDelete removes a pending-delete entry after a successful delete.
func (d *NamespacedResourceDispatcher) DeletePendingDelete(key string) {
	d.pendingDeletes.Delete(key)
}

// --- Pod handlers ---

func (d *NamespacedResourceDispatcher) onPodAdd(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	ctrls := d.getControllers(pod.Namespace)
	if ctrls == nil || ctrls.Pod == nil {
		return
	}
	ctrls.Pod.Reconcile(pod.Namespace + "/" + pod.Name)
}

func (d *NamespacedResourceDispatcher) onPodUpdate(oldObj, newObj any) {
	oldPod, ok := oldObj.(*corev1.Pod)
	if !ok {
		return
	}
	newPod, ok := newObj.(*corev1.Pod)
	if !ok {
		return
	}
	if oldPod.Annotations[ovntypes.OvnPodAnnotationName] == newPod.Annotations[ovntypes.OvnPodAnnotationName] &&
		oldPod.Spec.NodeName == newPod.Spec.NodeName {
		return
	}
	ctrls := d.getControllers(newPod.Namespace)
	if ctrls == nil || ctrls.Pod == nil {
		return
	}
	ctrls.Pod.Reconcile(newPod.Namespace + "/" + newPod.Name)
}

func (d *NamespacedResourceDispatcher) onPodDelete(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			return
		}
	}
	ctrls := d.getControllers(pod.Namespace)
	if ctrls == nil || ctrls.Pod == nil {
		return
	}
	key := pod.Namespace + "/" + pod.Name
	d.pendingDeletes.Store(key, pod)
	ctrls.Pod.Reconcile(key)
}

// --- Namespace handlers ---

func (d *NamespacedResourceDispatcher) onNamespaceAdd(obj any) {
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		return
	}
	ctrls := d.getControllers(ns.Name)
	if ctrls == nil || ctrls.Namespace == nil {
		return
	}
	ctrls.Namespace.Reconcile(ns.Name)
}

func (d *NamespacedResourceDispatcher) onNamespaceUpdate(oldObj, newObj any) {
	ns, ok := newObj.(*corev1.Namespace)
	if !ok {
		return
	}
	ctrls := d.getControllers(ns.Name)
	if ctrls == nil || ctrls.Namespace == nil {
		return
	}
	ctrls.Namespace.Reconcile(ns.Name)
}

func (d *NamespacedResourceDispatcher) onNamespaceDelete(obj any) {
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		ns, ok = tombstone.Obj.(*corev1.Namespace)
		if !ok {
			return
		}
	}
	ctrls := d.getControllers(ns.Name)
	if ctrls == nil || ctrls.Namespace == nil {
		return
	}
	ctrls.Namespace.Reconcile(ns.Name)
}

// --- NetworkPolicy handlers ---

func (d *NamespacedResourceDispatcher) onNetPolAdd(obj any) {
	np, ok := obj.(*knet.NetworkPolicy)
	if !ok {
		return
	}
	ctrls := d.getControllers(np.Namespace)
	if ctrls == nil || ctrls.NetPol == nil {
		return
	}
	ctrls.NetPol.Reconcile(np.Namespace + "/" + np.Name)
}

func (d *NamespacedResourceDispatcher) onNetPolUpdate(oldObj, newObj any) {
	oldNP, ok := oldObj.(*knet.NetworkPolicy)
	if !ok {
		return
	}
	newNP, ok := newObj.(*knet.NetworkPolicy)
	if !ok {
		return
	}
	if apiequality.Semantic.DeepEqual(oldNP.Spec, newNP.Spec) &&
		oldNP.Annotations[ovnStatelessNetPolAnnotation] == newNP.Annotations[ovnStatelessNetPolAnnotation] {
		return
	}
	ctrls := d.getControllers(newNP.Namespace)
	if ctrls == nil || ctrls.NetPol == nil {
		return
	}
	ctrls.NetPol.Reconcile(newNP.Namespace + "/" + newNP.Name)
}

func (d *NamespacedResourceDispatcher) onNetPolDelete(obj any) {
	np, ok := obj.(*knet.NetworkPolicy)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		np, ok = tombstone.Obj.(*knet.NetworkPolicy)
		if !ok {
			return
		}
	}
	ctrls := d.getControllers(np.Namespace)
	if ctrls == nil || ctrls.NetPol == nil {
		return
	}
	ctrls.NetPol.Reconcile(np.Namespace + "/" + np.Name)
}
