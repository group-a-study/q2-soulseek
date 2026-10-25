package power

import (
	"os"
	"path/filepath"
	"testing"
)

// The level is the CW2015's, as the Q2's status bar shows it; the battery supply's only without
// the gauge or with something else in it.
func TestRead(t *testing.T) {
	Root, Gauge = t.TempDir(), filepath.Join(t.TempDir(), "cw2015_capacity")
	write := func(p, v string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(v+"\n"), 0o644)
	}
	write(filepath.Join(Root, "axp2101_charger/type"), "USB")
	write(filepath.Join(Root, "axp2101_charger/online"), "0")
	write(filepath.Join(Root, "battery/type"), "Battery")
	write(filepath.Join(Root, "battery/status"), "Discharging")
	write(filepath.Join(Root, "battery/capacity"), "41")
	if s := Read(); s.Capacity != 41 || s.External || s.Status != "Discharging" {
		t.Fatalf("no gauge: %+v", s)
	}
	write(Gauge, "67")
	if s := Read(); s.Capacity != 67 {
		t.Fatalf("the gauge: %+v", s)
	}
	write(Gauge, "x")
	if s := Read(); s.Capacity != 41 {
		t.Fatalf("a bad gauge: %+v", s)
	}
	// Charge Limit holding 80% on the cable: the charger's online flag, not the battery charging.
	write(filepath.Join(Root, "axp2101_charger/online"), "1")
	write(filepath.Join(Root, "battery/status"), "Not charging")
	if s := Read(); !s.External {
		t.Fatalf("on the cable: %+v", s)
	}
}
