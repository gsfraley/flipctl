// Package assistant defines the provider-neutral interface the daemon uses
// when a ViewApp action asks an assistant to answer, revise the view, or act.
//
// The daemon never gives a provider a privileged path: a "revise" reply is a
// manifest that goes back through plugin.Parse and the registration gate like
// any other upload, and "act" is limited to what the reply can express.
// Concrete providers (Anthropic, OpenAI, any OpenAI-compatible local endpoint)
// plug in behind Provider; this package ships only a deterministic scripted
// provider used by tests and the demo.
package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Mode is what the user asked the assistant to do.
type Mode string

const (
	ModeAsk    Mode = "ask"
	ModeRevise Mode = "revise"
	ModeAct    Mode = "act"
)

// Request carries a prompt and the view state the action chose to share.
type Request struct {
	Mode     Mode              `json:"mode"`
	Prompt   string            `json:"prompt"`
	AppID    string            `json:"appId"`
	Manifest string            `json:"manifest,omitempty"`
	Form     map[string]string `json:"form,omitempty"`
	LastJob  *JobContext       `json:"lastJob,omitempty"`
}

// JobContext is the bounded job output shared with the provider.
type JobContext struct {
	AppID  string `json:"appId"`
	State  string `json:"state"`
	Output string `json:"output"`
}

// Reply is the provider's answer. Manifest is only honored for ModeRevise.
type Reply struct {
	Text     string `json:"text"`
	Manifest []byte `json:"-"`
}

// Provider is implemented by each assistant backend.
type Provider interface {
	// Name is a stable identifier recorded in registration sources.
	Name() string
	// DisplayName is what the device shows, e.g. "Claude" or "ChatGPT".
	DisplayName() string
	Complete(ctx context.Context, req Request) (Reply, error)
}

// Scripted is a deterministic offline provider. It answers "ask" by
// summarizing numeric fields in JSON-lines job output, which is enough to
// exercise the full loop without a model or network access.
type Scripted struct{ Label string }

func (s Scripted) Name() string { return "scripted" }

func (s Scripted) DisplayName() string {
	if s.Label != "" {
		return s.Label
	}
	return "Scripted assistant"
}

func (s Scripted) Complete(ctx context.Context, req Request) (Reply, error) {
	switch req.Mode {
	case ModeAsk:
		if req.LastJob == nil || strings.TrimSpace(req.LastJob.Output) == "" {
			return Reply{Text: "There is no captured data yet. Run a capture first."}, nil
		}
		return Reply{Text: SummarizeJSONLines(req.LastJob.Output)}, nil
	case ModeRevise, ModeAct:
		return Reply{Text: fmt.Sprintf("The scripted provider cannot %s; configure a model provider for this.", req.Mode)}, nil
	}
	return Reply{}, fmt.Errorf("unknown mode %q", req.Mode)
}

// SummarizeJSONLines reports count, mean and range for numeric fields, and the
// strongest sample when an "rssi" field is present.
func SummarizeJSONLines(out string) string {
	var rows []map[string]any
	for _, line := range strings.Split(out, "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &m) == nil {
			rows = append(rows, m)
		}
	}
	if len(rows) == 0 {
		return "The output contains no JSON records to analyze."
	}
	stats := map[string][]float64{}
	for _, r := range rows {
		for k, v := range r {
			if f, ok := v.(float64); ok {
				stats[k] = append(stats[k], f)
			}
		}
	}
	var parts []string
	if vals, ok := stats["rssi"]; ok && len(vals) > 0 {
		best := 0
		for i, r := range rows {
			if f, ok := r["rssi"].(float64); ok {
				if bf, _ := rows[best]["rssi"].(float64); f > bf {
					best = i
				}
			}
		}
		b := rows[best]
		parts = append(parts, fmt.Sprintf("Strongest packet: seq %s at %s dBm", num(b["seq"]), num(b["rssi"])))
		if snr, ok := b["snr"]; ok {
			parts[len(parts)-1] += fmt.Sprintf(" (SNR %s dB)", num(snr))
		}
		if lat, ok := b["lat"]; ok {
			parts[len(parts)-1] += fmt.Sprintf(" near %s, %s", num(lat), num(b["lon"]))
		}
	}
	keys := make([]string, 0, len(stats))
	for k := range stats {
		if k != "seq" && k != "lat" && k != "lon" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := stats[k]
		lo, hi, sum := math.Inf(1), math.Inf(-1), 0.0
		for _, f := range v {
			lo, hi, sum = math.Min(lo, f), math.Max(hi, f), sum+f
		}
		parts = append(parts, fmt.Sprintf("%s mean %s (range %s to %s)", k, num(math.Round(sum/float64(len(v))*10)/10), num(lo), num(hi)))
	}
	return fmt.Sprintf("Analyzed %d records. %s.", len(rows), strings.Join(parts, "; "))
}

func num(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}
