package sysstats

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Captured from jacks-desktop's RTX 3060 (Windows, so CRLF), plus a card
// that reports no power draw.
const nvidiaSMIOutput = "NVIDIA GeForce RTX 3060, 1, 371, 12288, 45, 33.91\r\n" +
	"NVIDIA T400, 0, 5, 2048, 38, [N/A]\r\n"

func TestParseNvidiaSMI(t *testing.T) {
	got := parseNvidiaSMI([]byte(nvidiaSMIOutput))
	want := []GPU{
		{Name: "NVIDIA GeForce RTX 3060", Percent: 1, MemoryUsed: 371 << 20, MemoryTotal: 12288 << 20, Celsius: 45, Watts: 33.91},
		{Name: "NVIDIA T400", MemoryUsed: 5 << 20, MemoryTotal: 2048 << 20, Celsius: 38},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d cards, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("card %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Without nvidia-smi there are no GPUs, and the reading leaves the field out.
func TestNoNvidiaSMI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if gpus, _ := readSensors(context.Background()); gpus != nil {
		t.Fatalf("gpus = %+v, want none", gpus)
	}
	out, err := json.Marshal(Stats{})
	if err != nil || strings.Contains(string(out), `"gpus"`) {
		t.Fatalf("reading without GPUs = %s, %v; want no gpus field", out, err)
	}
}
