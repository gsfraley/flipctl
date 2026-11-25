package assistant

import (
	"context"
	"strings"
	"testing"
)

func TestScriptedProviderSummarizesTelemetry(t *testing.T) {
	out := `{"seq":1,"rssi":-80,"snr":5.5,"lat":59.9,"lon":30.3}
{"seq":2,"rssi":-62,"snr":9.1,"lat":59.91,"lon":30.31}
not json
{"seq":3,"rssi":-71,"snr":7,"lat":59.92,"lon":30.32}`
	r, err := Scripted{}.Complete(context.Background(), Request{Mode: ModeAsk, LastJob: &JobContext{Output: out}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Analyzed 3 records", "seq 2 at -62 dBm (SNR 9.1 dB) near 59.91, 30.31", "rssi mean -71 (range -80 to -62)"} {
		if !strings.Contains(r.Text, want) {
			t.Fatalf("missing %q in %q", want, r.Text)
		}
	}
	r, _ = Scripted{}.Complete(context.Background(), Request{Mode: ModeAsk})
	if !strings.Contains(r.Text, "no captured data") {
		t.Fatalf("empty context reply %q", r.Text)
	}
	if r, _ := (Scripted{}).Complete(context.Background(), Request{Mode: ModeRevise}); r.Manifest != nil {
		t.Fatal("scripted provider must not produce manifests")
	}
}
