// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ovn-kubernetes/libovsdb/cache"
	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	kexec "k8s.io/utils/exec"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops/ovs"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/telemetry"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

var runner kexec.Interface
var vsctlPath string
var ofctlPath string
var ovsDBClient libovsdbclient.Client

// SetOVSDBClient sets the package-level libovsdb client for OVS operations,
// avoiding fork/exec of ovs-vsctl.
func SetOVSDBClient(c libovsdbclient.Client) {
	ovsDBClient = c
}

type ovsPortEvent struct {
	installed bool
	active    bool
}

type ovsPortWaiter struct {
	ifaceID string
	ch      chan ovsPortEvent
}

// ovnInstalledWatcher routes libovsdb Interface cache updates to CNI
// goroutines waiting for ovn-installed. Single global handler, O(1) dispatch.
type ovnInstalledWatcher struct {
	waiters sync.Map // ifaceName → *ovsPortWaiter
}

var portWatcher *ovnInstalledWatcher

func initPortWatcher(c libovsdbclient.Client) {
	portWatcher = &ovnInstalledWatcher{}
	c.Cache().AddEventHandler(&cache.EventHandlerFuncs{
		UpdateFunc: func(_ string, _ model.Model, newModel model.Model) {
			iface, ok := newModel.(*vswitchd.Interface)
			if !ok {
				return
			}
			val, ok := portWatcher.waiters.Load(iface.Name)
			if !ok {
				return
			}
			waiter := val.(*ovsPortWaiter)
			if iface.ExternalIDs["iface-id"] != waiter.ifaceID {
				select {
				case waiter.ch <- ovsPortEvent{active: false}:
				default:
				}
				return
			}
			if iface.ExternalIDs["ovn-installed"] == "true" {
				select {
				case waiter.ch <- ovsPortEvent{installed: true, active: true}:
				default:
				}
			}
		},
		DeleteFunc: func(_ string, m model.Model) {
			iface, ok := m.(*vswitchd.Interface)
			if !ok {
				return
			}
			val, ok := portWatcher.waiters.Load(iface.Name)
			if !ok {
				return
			}
			waiter := val.(*ovsPortWaiter)
			select {
			case waiter.ch <- ovsPortEvent{active: false}:
			default:
			}
		},
	})
}

func SetExec(r kexec.Interface) error {
	runner = r
	var err error
	vsctlPath, err = r.LookPath("ovs-vsctl")
	if err != nil {
		return err
	}
	ofctlPath, err = r.LookPath("ovs-ofctl")
	return err
}

// ResetRunner used by unit-tests to reset runner to its initial (un-initialized) value
func ResetRunner() {
	runner = nil
}

func ovsExec(args ...string) (string, error) {
	if runner == nil {
		return "", fmt.Errorf("OVS exec runner not initialized")
	}

	args = append([]string{"--timeout=30"}, args...)
	output, err := runner.Command(vsctlPath, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run 'ovs-vsctl %s': %v\n  %q", strings.Join(args, " "), err, string(output))
	}

	return strings.TrimSuffix(string(output), "\n"), nil
}

// ovsGetMultiOutput allows running get command with multiple columns
// returns a slice of requested fields
// if row doesn't exist command output will be a slice with an empty string
// empty ovsdb value [] will be replaced with an empty string
func ovsGetMultiOutput(table, record string, columns []string) ([]string, error) {
	args := []string{"--if-exists", "get", table, record}
	args = append(args, columns...)
	output, err := ovsExec(args...)
	var result []string
	// columns are separated with \n
	// remove \" as with --data=bare formatting
	for _, column := range strings.Split(strings.ReplaceAll(output, "\"", ""), "\n") {
		if column == "[]" {
			result = append(result, "")
		} else {
			result = append(result, column)
		}
	}
	return result, err
}

func ovsCreate(table string, values ...string) (string, error) {
	args := append([]string{"create", table}, values...)
	return ovsExec(args...)
}

func ovsDestroy(table, record string) error {
	_, err := ovsExec("--if-exists", "destroy", table, record)
	return err
}

func ovsSet(table, record string, values ...string) error {
	args := append([]string{"set", table, record}, values...)
	_, err := ovsExec(args...)
	return err
}

func getDatapathType(bridge string) (string, error) {
	br_type, err := ovsGet("bridge", bridge, "datapath_type", "")
	if err != nil {
		return "", err
	}
	return br_type, nil
}

func ovsGet(table, record, column, key string) (string, error) {
	args := []string{"--if-exists", "get", table, record}
	if key != "" {
		args = append(args, fmt.Sprintf("%s:%s", column, key))
	} else {
		args = append(args, column)
	}
	output, err := ovsExec(args...)
	return strings.Trim(strings.TrimSpace(string(output)), "\""), err
}

// Returns the given column of records that match the condition
func ovsFind(table, column string, conditions ...string) ([]string, error) {
	args := append([]string{"--no-heading", "--format=csv", "--data=bare", "--columns=" + column, "find", table}, conditions...)
	output, err := ovsExec(args...)
	if err != nil {
		return nil, err
	}
	if output == "" {
		return nil, nil
	}
	return strings.Split(output, "\n"), nil
}

func ovsClear(table, record string, columns ...string) error {
	args := append([]string{"--if-exists", "clear", table, record}, columns...)
	_, err := ovsExec(args...)
	return err
}

func ofctlExec(args ...string) (string, error) {
	if runner == nil {
		return "", fmt.Errorf("OVS exec runner not initialized")
	}

	args = append([]string{"--timeout=10", "--no-stats", "--strict"}, args...)
	var stdout, stderr bytes.Buffer
	cmd := runner.Command(ofctlPath, args...)
	cmd.SetStdout(&stdout)
	cmd.SetStderr(&stderr)

	cmdStr := strings.Join(args, " ")
	klog.V(5).Infof("Exec: %s %s", ofctlPath, cmdStr)

	err := cmd.Run()
	if err != nil {
		stderrStr := stderr.String()
		klog.Errorf("Exec: %s %s : stderr: %q", ofctlPath, cmdStr, stderrStr)
		return "", fmt.Errorf("failed to run '%s %s': %v\n  %q", ofctlPath, cmdStr, err, stderrStr)
	}
	stdoutStr := stdout.String()
	klog.V(5).Infof("Exec: %s %s: stdout: %q", ofctlPath, cmdStr, stdoutStr)

	trimmed := strings.TrimSpace(stdoutStr)
	// If output is a single line, strip the trailing newline
	if strings.Count(trimmed, "\n") == 0 {
		stdoutStr = trimmed
	}
	return stdoutStr, nil
}

// checkCancelSandbox checks that this sandbox is still valid for the current
// instance of the pod in the apiserver. Sandbox requests and pod instances
// have a 1:1 relationship determined by pod UID. If we detect that the pod
// has changed either UID or MAC terminate this sandbox request early instead
// of waiting for OVN to set up flows that will never exist.
func checkCancelSandbox(mac string, getter PodInfoGetter, namespace, name, nadKey, initialPodUID string) error {
	// Not all node CNI modes may have access to kube api, those will pass nil as getter.
	if getter == nil {
		return nil
	}
	pod, err := getter.getPod(namespace, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("pod deleted")
		}
		klog.Warningf("[%s/%s] failed to get pod while waiting for OVS port binding: %v", namespace, name, err)
		return nil
	}

	if string(pod.UID) != initialPodUID {
		// Pod UID changed and this sandbox should be canceled
		// so the new pod sandbox can run
		return fmt.Errorf("canceled old pod sandbox")
	}

	ovnAnnot, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
	if err != nil {
		return fmt.Errorf("pod OVN annotations deleted or invalid")
	}

	// Pod OVN annotation changed and this sandbox should
	// be canceled so the new pod sandbox can run with the
	// updated MAC/IP
	if mac != ovnAnnot.MAC.String() {
		return fmt.Errorf("pod OVN annotations changed")
	}

	return nil
}

func getInterfaceOVNInstalled(ifaceName, ifaceID string) (installed bool, active bool, err error) {
	ifaces, lookupErr := ovs.FindInterfacesWithPredicate(ovsDBClient, func(i *vswitchd.Interface) bool {
		return i.Name == ifaceName
	})
	if lookupErr != nil {
		return false, false, lookupErr
	}
	if len(ifaces) == 0 {
		return false, true, nil
	}
	iface := ifaces[0]
	currentIfaceID := iface.ExternalIDs["iface-id"]
	if currentIfaceID != ifaceID {
		return false, false, nil
	}
	return iface.ExternalIDs["ovn-installed"] == "true", true, nil
}

func waitForPodInterface(ctx context.Context, ifInfo *PodInterfaceInfo,
	ifaceName, ifaceID string, getter PodInfoGetter,
	namespace, name, initialPodUID string) error {
	waitStart := time.Now()
	mac := ifInfo.MAC.String()
	ifAddrs := ifInfo.IPs

	installed, active, err := getInterfaceOVNInstalled(ifaceName, ifaceID)
	if err == nil && installed {
		emitOVNInstalled(waitStart, namespace, name, ifInfo, ifaceName)
		return nil
	}
	if err == nil && !active {
		return fmt.Errorf("OVS sandbox port %s is no longer active (probably due to a subsequent CNI ADD)", ifaceName)
	}

	waiter := &ovsPortWaiter{
		ifaceID: ifaceID,
		ch:      make(chan ovsPortEvent, 1),
	}
	portWatcher.waiters.Store(ifaceName, waiter)
	defer portWatcher.waiters.Delete(ifaceName)

	// Re-check after registration to close race window.
	installed, active, err = getInterfaceOVNInstalled(ifaceName, ifaceID)
	if err == nil && installed {
		emitOVNInstalled(waitStart, namespace, name, ifInfo, ifaceName)
		return nil
	}
	if err == nil && !active {
		return fmt.Errorf("OVS sandbox port %s is no longer active (probably due to a subsequent CNI ADD)", ifaceName)
	}

	cancelTicker := time.NewTicker(200 * time.Millisecond)
	defer cancelTicker.Stop()

	for {
		select {
		case ev := <-waiter.ch:
			if !ev.active {
				return fmt.Errorf("OVS sandbox port %s is no longer active (probably due to a subsequent CNI ADD)", ifaceName)
			}
			if ev.installed {
				emitOVNInstalled(waitStart, namespace, name, ifInfo, ifaceName)
				return nil
			}
		case <-cancelTicker.C:
			if err := checkCancelSandbox(mac, getter, namespace, name, ifInfo.NADKey, initialPodUID); err != nil {
				return fmt.Errorf("%v waiting for OVS port binding for %s %v", err, mac, ifAddrs)
			}
		case <-ctx.Done():
			errDetail := "timed out"
			if ctx.Err() == context.Canceled {
				errDetail = "canceled while"
			}
			telemetry.Emit(telemetry.Event{
				Event:   "cni_ovn_installed_failed",
				Pod:     namespace + "/" + name,
				Network: ifInfo.NetName,
				Elapsed: float64(time.Since(waitStart).Microseconds()) / 1000.0,
				Detail:  map[string]any{"iface": ifaceName, "err": errDetail},
			})
			return fmt.Errorf("%s waiting for OVS port binding (ovn-installed) for %s %v", errDetail, mac, ifAddrs)
		}
	}
}

func emitOVNInstalled(waitStart time.Time, namespace, name string, ifInfo *PodInterfaceInfo, ifaceName string) {
	telemetry.Emit(telemetry.Event{
		Event:   "cni_ovn_installed",
		Pod:     namespace + "/" + name,
		Network: ifInfo.NetName,
		Elapsed: float64(time.Since(waitStart).Microseconds()) / 1000.0,
		Detail:  map[string]any{"iface": ifaceName},
	})
}
