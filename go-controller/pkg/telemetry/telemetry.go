package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

var enabled = os.Getenv("OVN_TELEMETRY") != "0"

type Event struct {
	TS      string         `json:"ts"`
	Event   string         `json:"event"`
	PodUID  string         `json:"pod_uid,omitempty"`
	Pod     string         `json:"pod,omitempty"`
	Svc     string         `json:"svc,omitempty"`
	Network string         `json:"network,omitempty"`
	Topo    string         `json:"topology,omitempty"`
	Role    string         `json:"role,omitempty"`
	Node    string         `json:"node,omitempty"`
	Elapsed float64        `json:"elapsed_ms,omitempty"`
	Detail  map[string]any `json:"detail,omitempty"`
}

func Emit(e Event) {
	if !enabled {
		return
	}
	e.TS = time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(e)
	fmt.Println(string(b))
}

func EmitAt(t time.Time, e Event) {
	if !enabled {
		return
	}
	e.TS = t.UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(e)
	fmt.Println(string(b))
}

func Enabled() bool {
	return enabled
}
