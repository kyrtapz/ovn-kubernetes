// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package iprulemanager

import (
	"fmt"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

type Interface interface {
	Run(stopCh <-chan struct{}, syncPeriod time.Duration)
	Add(rule netlink.Rule) error
	AddWithMetadata(rule netlink.Rule, metadata string) error
	Delete(rule netlink.Rule) error
	DeleteWithMetadata(metadata string) error
	OwnPriority(priority int) error
}

type managedRule struct {
	rule     *netlink.Rule
	metadata string
}

type Controller struct {
	mu            sync.Mutex
	rules         map[string]*managedRule     // ruleKey -> managed rule
	metadataIndex map[string]sets.Set[string] // metadata -> set of ruleKeys
	ownPriorities map[int]bool
	v4            bool
	v6            bool
	family        int
}

func NewController(v4, v6 bool) *Controller {
	family := netlink.FAMILY_ALL
	if v4 && !v6 {
		family = netlink.FAMILY_V4
	} else if v6 && !v4 {
		family = netlink.FAMILY_V6
	}
	return &Controller{
		rules:         make(map[string]*managedRule),
		metadataIndex: make(map[string]sets.Set[string]),
		ownPriorities: make(map[int]bool),
		v4:            v4,
		v6:            v6,
		family:        family,
	}
}

func (rm *Controller) Run(stopCh <-chan struct{}, syncPeriod time.Duration) {
	ticker := time.NewTicker(syncPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			rm.mu.Lock()
			if err := rm.reconcile(); err != nil {
				klog.Errorf("IP Rule manager: failed to reconcile (retry in %s): %v", syncPeriod.String(), err)
			}
			rm.mu.Unlock()
		}
	}
}

func (rm *Controller) Add(rule netlink.Rule) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	key := ruleKey(&rule)
	if _, exists := rm.rules[key]; exists {
		return nil
	}

	if err := netlink.RuleAdd(&rule); err != nil && !isEEXIST(err) {
		return err
	}
	rm.rules[key] = &managedRule{rule: &rule}
	return nil
}

func (rm *Controller) AddWithMetadata(rule netlink.Rule, metadata string) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	key := ruleKey(&rule)
	if _, exists := rm.rules[key]; exists {
		return nil
	}

	if err := netlink.RuleAdd(&rule); err != nil && !isEEXIST(err) {
		return err
	}
	rm.rules[key] = &managedRule{rule: &rule, metadata: metadata}
	if metadata != "" {
		if rm.metadataIndex[metadata] == nil {
			rm.metadataIndex[metadata] = sets.New[string]()
		}
		rm.metadataIndex[metadata].Insert(key)
	}
	return nil
}

func (rm *Controller) Delete(rule netlink.Rule) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	key := ruleKey(&rule)
	mr, exists := rm.rules[key]
	if !exists {
		return nil
	}

	if err := netlink.RuleDel(&rule); err != nil && !isENOENT(err) {
		return err
	}
	rm.removeFromIndex(key, mr.metadata)
	delete(rm.rules, key)
	return nil
}

func (rm *Controller) DeleteWithMetadata(metadata string) error {
	if metadata == "" {
		return nil
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()

	keys, exists := rm.metadataIndex[metadata]
	if !exists {
		return nil
	}

	var errors []error
	for key := range keys {
		mr := rm.rules[key]
		if mr == nil {
			continue
		}
		if err := netlink.RuleDel(mr.rule); err != nil && !isENOENT(err) {
			errors = append(errors, err)
			continue
		}
		delete(rm.rules, key)
	}
	delete(rm.metadataIndex, metadata)
	return utilerrors.Join(errors...)
}

func (rm *Controller) OwnPriority(priority int) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.ownPriorities[priority] = true
	return rm.reconcile()
}

// reconcile ensures kernel state matches desired state. Runs periodically, not per-mutation.
func (rm *Controller) reconcile() error {
	start := time.Now()
	defer func() {
		klog.V(5).Infof("Reconciling IP rules took %v", time.Since(start))
	}()

	rulesFound, err := netlink.RuleList(rm.family)
	if err != nil {
		return err
	}

	// Build kernel rule set for O(1) lookups
	kernelSet := make(map[string]*netlink.Rule, len(rulesFound))
	for i := range rulesFound {
		kernelSet[ruleKey(&rulesFound[i])] = &rulesFound[i]
	}

	var errors []error

	// Restore managed rules missing from kernel
	for key, mr := range rm.rules {
		if _, inKernel := kernelSet[key]; !inKernel {
			if err := netlink.RuleAdd(mr.rule); err != nil && !isEEXIST(err) {
				errors = append(errors, err)
			}
		}
	}

	// Remove stale rules at owned priorities
	for i := range rulesFound {
		r := &rulesFound[i]
		if !rm.ownPriorities[r.Priority] {
			continue
		}
		key := ruleKey(r)
		if _, managed := rm.rules[key]; !managed {
			klog.Infof("Rule manager: deleting stale IP rule (%s) found at priority %d", r.String(), r.Priority)
			if err := netlink.RuleDel(r); err != nil {
				errors = append(errors, fmt.Errorf("failed to delete stale IP rule (%s) found at priority %d: %v",
					r.String(), r.Priority, err))
			}
		}
	}

	return utilerrors.Join(errors...)
}

func (rm *Controller) removeFromIndex(key, metadata string) {
	if metadata == "" {
		return
	}
	if s, ok := rm.metadataIndex[metadata]; ok {
		s.Delete(key)
		if s.Len() == 0 {
			delete(rm.metadataIndex, metadata)
		}
	}
}

func ruleKey(r *netlink.Rule) string {
	srcStr := "<nil>"
	if r.Src != nil {
		srcStr = r.Src.String()
	}
	dstStr := "<nil>"
	if r.Dst != nil {
		dstStr = r.Dst.String()
	}
	return fmt.Sprintf("%d|%d|%d|%d|%s|%s", r.Priority, r.Table, r.Type, r.Mark, srcStr, dstStr)
}

func areNetlinkRulesEqual(r1, r2 *netlink.Rule) bool {
	if r1.Priority != r2.Priority {
		return false
	}
	if r1.Table != r2.Table {
		return false
	}
	if r1.Type != r2.Type {
		return false
	}
	if r1.Mark != r2.Mark {
		return false
	}

	return areIPNetsEqual(r1.Src, r2.Src) && areIPNetsEqual(r1.Dst, r2.Dst)
}

func areIPNetsEqual(n1, n2 *net.IPNet) bool {
	if n1 == nil && n2 == nil {
		return true
	}
	if n1 == nil || n2 == nil {
		return false
	}

	if !n1.IP.Equal(n2.IP) {
		return false
	}

	n1ones, n1bits := n1.Mask.Size()
	n2ones, n2bits := n2.Mask.Size()
	return n1ones == n2ones && n1bits == n2bits
}

func isNetlinkRuleInSlice(rules []netlink.Rule, candidate *netlink.Rule) (bool, *netlink.Rule) {
	for _, r := range rules {
		r := r
		if areNetlinkRulesEqual(&r, candidate) {
			return true, &r
		}
	}
	return false, netlink.NewRule()
}

func isEEXIST(err error) bool {
	return err == syscall.EEXIST
}

func isENOENT(err error) bool {
	return err == syscall.ENOENT
}
