// SPDX-License-Identifier: Apache-2.0

package update

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// StateFile is the name of the persisted result of the last background check, inside the state directory.
const StateFile = "update.json"

// State is what the background check remembers; status, doctor and the UI read it to tell the user.
type State struct {
	Checked   time.Time `json:"checked"`
	Next      time.Time `json:"next"` // earliest time of the next check (about a day later, jittered)
	Latest    string    `json:"latest,omitempty"`
	URL       string    `json:"url,omitempty"`
	Urgent    bool      `json:"urgent,omitempty"`
	Published time.Time `json:"published,omitempty"`
	Notified  string    `json:"notified,omitempty"` // the version an event was last written for
}

// ReadState loads the state; a missing or unreadable file is an empty state (the check simply has not run).
func ReadState(dir string) State {
	// #nosec G304 -- our own state directory
	b, err := os.ReadFile(filepath.Join(dir, StateFile)) //nolint:gosec // see #nosec above
	if err != nil {
		return State{}
	}
	var s State
	if json.Unmarshal(b, &s) != nil {
		return State{}
	}
	return s
}

// WriteState stores the state atomically with private permissions.
func WriteState(dir string, s State) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".update-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, StateFile)); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Available reports whether the remembered latest release is newer than the running version. A running version that
// is not a release (a development build) never sees one.
func (s State) Available(current string) bool {
	cur, err := ParseVersion(current)
	if err != nil || s.Latest == "" {
		return false
	}
	latest, err := ParseVersion(s.Latest)
	return err == nil && latest.Compare(cur) > 0
}
