// Package settings keeps what the person using this device has chosen: its
// name, where received files go, and which paired devices need no asking.
package settings

import (
	"encoding/json"
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Settings is the content of settings.json.
type Settings struct {
	// Name is how this device appears to others.
	Name string `json:"name"`
	// SaveDir is where received files go.
	SaveDir string `json:"saveDir"`
	// AlwaysAccept holds the fingerprints of paired devices whose files
	// are saved without asking.
	AlwaysAccept []string `json:"alwaysAccept,omitempty"`
	// HideNearby is whether this device stays away from the meeting point
	// where devices on the same network find each other. Shown unless
	// the person turns it off.
	HideNearby bool `json:"hideNearby,omitempty"`
	// Labels holds the owner's own names for paired devices, by
	// fingerprint. They stay on this device.
	Labels map[string]string `json:"labels,omitempty"`
	// NoUpdateCheck is whether Yonder leaves off asking its site if a
	// newer version is out. It asks unless the person turns it off.
	NoUpdateCheck bool `json:"noUpdateCheck,omitempty"`
	// SkipVersion is the newest version the person was told of and put
	// away; only one newer than it is told of again.
	SkipVersion string `json:"skipVersion,omitempty"`
}

func path(dir string) string { return filepath.Join(dir, "settings.json") }

// Load reads the settings in dir. On first run it makes them, with a random
// name and the Downloads folder, and saves them so the name stays.
func Load(dir string) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(path(dir))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return s, err
	default:
		if err := json.Unmarshal(b, &s); err != nil {
			return s, err
		}
	}
	changed := false
	if strings.TrimSpace(s.Name) == "" {
		s.Name, changed = RandomName(), true
	}
	if s.SaveDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return s, err
		}
		s.SaveDir, changed = filepath.Join(home, "Downloads"), true
	}
	if changed {
		return s, s.Save(dir)
	}
	return s, nil
}

// Save writes the settings to dir.
func (s Settings) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path(dir), b, 0o600)
}

// Always reports whether files from the device are saved without asking.
func (s Settings) Always(fingerprint string) bool {
	return slices.Contains(s.AlwaysAccept, fingerprint)
}

// SetAlways turns asking off or back on for a device.
func (s *Settings) SetAlways(fingerprint string, always bool) {
	s.AlwaysAccept = slices.DeleteFunc(s.AlwaysAccept, func(f string) bool { return f == fingerprint })
	if always {
		s.AlwaysAccept = append(s.AlwaysAccept, fingerprint)
	}
}

// SetLabel gives a device the owner's own name, or with "" takes it away.
func (s *Settings) SetLabel(fingerprint, label string) {
	if label == "" {
		delete(s.Labels, fingerprint)
		return
	}
	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	s.Labels[fingerprint] = label
}

var (
	adjectives = []string{"Amber", "Bright", "Calm", "Clever", "Distant", "Gentle", "Golden", "Hidden", "Keen", "Lively", "Mellow", "Misty", "Nimble", "Quiet", "Rapid", "Silver", "Steady", "Sunny", "Swift", "Tidy", "Vivid", "Wandering", "Warm", "Wild"}
	nouns      = []string{"Badger", "Beacon", "Brook", "Cedar", "Comet", "Falcon", "Fern", "Harbour", "Heron", "Juniper", "Lantern", "Lark", "Meadow", "Otter", "Pebble", "Pine", "Plover", "Ridge", "Robin", "Sparrow", "Summit", "Thistle", "Willow", "Wren"}
)

// RandomName makes a two-word device name, as LocalSend does.
func RandomName() string {
	return adjectives[rand.IntN(len(adjectives))] + " " + nouns[rand.IntN(len(nouns))]
}
