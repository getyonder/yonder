package gui

import "errors"

// Only one Yonder runs at a time: a second one started hands over to the
// first and exits. When the second is newer, the person meant to run the
// new version, so the first says that an older one is still running.

// Started tells this Yonder that another copy was started and has handed
// over to it; version is that copy's, empty if it did not say. A newer
// one brings the notice.
func (s *Service) Started(version string) {
	s.mu.Lock()
	show := newer(version, s.version) && newer(version, s.started)
	if show {
		s.started = version
	}
	s.mu.Unlock()
	if show {
		s.changed()
	}
}

// startedNotice is the notice to show, if any. s.mu is held.
func (s *Service) startedNotice() *Update {
	if s.started == "" {
		return nil
	}
	return &Update{Version: s.started, Have: s.version}
}

// HideStarted puts the notice away.
func (s *Service) HideStarted() error {
	s.mu.Lock()
	s.started = ""
	s.mu.Unlock()
	s.changed()
	return nil
}

// Quit quits Yonder, so that a newer copy can be started.
func (s *Service) Quit() error {
	if s.quit == nil {
		return errors.New("This Yonder cannot quit by itself.")
	}
	go s.quit()
	return nil
}
