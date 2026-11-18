package gui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"

	"yonder/internal/tally/agent"
	"yonder/internal/withdrawn"
)

// The notice that this version of Yonder has been withdrawn. The site's
// file about the newest version also carries a signed list of withdrawn
// versions (package withdrawn). Yonder keeps the last list it accepted in
// its settings folder, so the notice is there after a restart with no
// network, and so an older list served again is not taken. The notice
// warns and cannot be put away; it does not stop Yonder. It goes when a
// newer list no longer names this version. A person who has the update
// check off makes no request and gets no notice.

// siteFile is the site's file about the newest version, and also the copy
// of it that is kept.
type siteFile struct {
	Version string `json:"version"`
	Page    string `json:"page"`
	withdrawn.Signed
}

func keptPath(dir string) string { return filepath.Join(dir, "withdrawn.json") }

// loadKept reads the kept copy in dir; false if there is none that can be
// read.
func loadKept(dir string) (siteFile, bool) {
	var f siteFile
	b, err := os.ReadFile(keptPath(dir))
	if err != nil || json.Unmarshal(b, &f) != nil {
		return siteFile{}, false
	}
	if !agent.KnownVersion(f.Version) {
		f.Version = ""
	}
	if !ownPage(f.Page) {
		f.Page = downloadPage
	}
	return f, true
}

// saveKept writes f as the kept copy in dir, whole or not at all.
func saveKept(dir string, f siteFile) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp := keptPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, keptPath(dir))
}

// takeList accepts the list in f if its signature agrees and it is not
// older than the one held; it reports whether it did. A file with no list
// changes nothing, so one who takes the site cannot take a notice away by
// leaving the list out. s.mu must be held.
func (s *Service) takeList(f siteFile) bool {
	list, err := f.Open(s.withdrawKey)
	if err != nil || list.Made.Before(s.list.Made) {
		return false
	}
	s.list = list
	return true
}

// restoreKept takes the list that was kept, when Yonder starts. If the
// list withdraws this version, the newest version that was known then is
// taken with it, so that the notice can name it before the site answers.
// It is for Start, before anything else has the Service.
func (s *Service) restoreKept() {
	f, ok := loadKept(s.dir)
	if !ok || !s.takeList(f) {
		return
	}
	if s.list.Has(s.version, system()) {
		s.latest, s.latestPage = f.Version, f.Page
	}
}

// system is this system as a list names it.
func system() string { return withdrawn.System(runtime.GOOS) }

// withdrawnNotice is the notice to show, if any: Have is this version, and
// Version the one to get in its place, empty if no newer version is out
// that is not withdrawn too. s.mu must be held.
func (s *Service) withdrawnNotice() *Update {
	if s.set.NoUpdateCheck || !s.list.Has(s.version, system()) {
		return nil
	}
	get := s.latest
	if !newer(get, s.version) || s.list.Has(get, system()) {
		get = ""
	}
	return &Update{Version: get, Have: s.version}
}
