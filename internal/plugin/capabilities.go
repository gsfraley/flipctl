package plugin

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Capabilities are the host effects a plugin declares it needs. They drive the
// sandbox policy for its commands and decide whether a runtime registration
// must be confirmed on the device before it becomes active.
type Capabilities struct {
	// Network allows the plugin's commands to reach the network. Without it,
	// sandboxed commands run in an empty network namespace.
	Network bool `json:"network,omitempty" yaml:"network"`
	// Devices lists the /dev nodes the plugin's commands may open.
	Devices []string `json:"devices,omitempty" yaml:"devices"`
	// RFTransmit marks plugins that can key a transmitter (Sub-GHz, LoRa, IR).
	RFTransmit bool `json:"rf_transmit,omitempty" yaml:"rf_transmit"`
	// Agent marks apps whose actions send on-device data to an assistant
	// provider. It is computed by the daemon, never declared.
	Agent bool `json:"agent,omitempty" yaml:"-"`
}

// Sensitive reports whether registering an app with these capabilities
// requires a local device confirmation.
func (c Capabilities) Sensitive() bool {
	return c.Network || len(c.Devices) > 0 || c.RFTransmit || c.Agent
}

// Covers reports whether c grants everything o asks for.
func (c Capabilities) Covers(o Capabilities) bool {
	if (o.Network && !c.Network) || (o.RFTransmit && !c.RFTransmit) || (o.Agent && !c.Agent) {
		return false
	}
	have := map[string]bool{}
	for _, d := range c.Devices {
		have[d] = true
	}
	for _, d := range o.Devices {
		if !have[d] {
			return false
		}
	}
	return true
}

// Union returns the capabilities granted by either c or o.
func (c Capabilities) Union(o Capabilities) Capabilities {
	out := Capabilities{Network: c.Network || o.Network, RFTransmit: c.RFTransmit || o.RFTransmit, Agent: c.Agent || o.Agent}
	seen := map[string]bool{}
	for _, d := range append(append([]string{}, c.Devices...), o.Devices...) {
		if !seen[d] {
			seen[d] = true
			out.Devices = append(out.Devices, d)
		}
	}
	sort.Strings(out.Devices)
	return out
}

// Names lists capabilities in a stable, human-readable form for views and logs.
func (c Capabilities) Names() []string {
	var out []string
	if c.Network {
		out = append(out, "network")
	}
	for _, d := range c.Devices {
		out = append(out, "device "+d)
	}
	if c.RFTransmit {
		out = append(out, "rf_transmit")
	}
	if c.Agent {
		out = append(out, "sends data to assistant")
	}
	return out
}

func (c Capabilities) String() string {
	if n := c.Names(); len(n) > 0 {
		return strings.Join(n, ", ")
	}
	return "none"
}

func validateCapabilities(c Capabilities) error {
	for _, d := range c.Devices {
		if !strings.HasPrefix(d, "/dev/") || path.Clean(d) != d || strings.Contains(d, "..") {
			return fmt.Errorf("invalid device capability %q", d)
		}
	}
	return nil
}
