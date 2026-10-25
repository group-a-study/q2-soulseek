// Package power reads the Q2's supplies from sysfs: the AXP2101's charger (axp2101_charger, its
// online flag), the battery's status, and the level from the CW2015 fuel gauge (q2-pod's
// docs/internals.md#charge-limit). Q2 Pod's Charge Limit turns charging off at 80% with the cable
// still in, so "on external power" is the charger's online flag, not the battery charging.
package power

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Root is sysfs's power supply class; tests point it elsewhere.
var Root = "/sys/class/power_supply"

// Gauge is the CW2015's level, the one the Q2's status bar shows: stock's toolsGetBatterycapacity
// reads it on the Q2 rather than the battery supply's capacity, the AXP2101's own estimate, which
// can be well off it. Tests point it elsewhere.
var Gauge = "/sys/devices/i2c-0/0-0062/cw2015_capacity"

type Status struct {
	External bool   // a cable powers the Q2 (charging or not)
	Status   string // the battery's: Charging, Discharging, Full, Not charging, or ""
	Capacity int    // 0-100, -1 unknown
}

func read(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// level is a percentage as sysfs writes it, -1 if it isn't one.
func level(s string) int {
	if c, err := strconv.Atoi(s); err == nil && c >= 0 && c <= 100 {
		return c
	}
	return -1
}

// Read looks at every supply once. Without a supply that reports online, a battery that says it is
// charging, full or not charging (plugged in, held by Charge Limit) counts as external power. The
// level is the gauge's, or the battery supply's without one.
func Read() Status {
	s := Status{Capacity: level(read(filepath.Dir(Gauge), filepath.Base(Gauge)))}
	dirs, _ := filepath.Glob(filepath.Join(Root, "*"))
	online := false
	for _, d := range dirs {
		if strings.EqualFold(read(d, "type"), "Battery") {
			s.Status = read(d, "status")
			if s.Capacity < 0 {
				s.Capacity = level(read(d, "capacity"))
			}
			continue
		}
		if v := read(d, "online"); v != "" {
			online = true
			s.External = s.External || v == "1"
		}
	}
	if !online {
		switch s.Status {
		case "Charging", "Full", "Not charging":
			s.External = true
		}
	}
	return s
}
