package pagewatch

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/runlock"
)

// DefaultRoot keeps each browser identity's watcher state outside the repository.
func DefaultRoot(identity brwidentity.Identity) (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "brw", "page-watchers", runlock.Key(identity)), nil
}

func (s *Service) store(id string, r *record) error {
	state := diskState{Version: 1, Watchers: make(map[string]record, len(s.records)+1)}
	for key, value := range s.records {
		state.Watchers[key] = value
	}
	if r == nil {
		delete(state.Watchers, id)
	} else {
		state.Watchers[id] = *r
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(data) > maxStateBytes {
		return errors.New("page watcher state exceeds size limit")
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".watchers-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), s.path)
	}
	if err == nil {
		dir, openErr := os.Open(filepath.Dir(s.path))
		if openErr != nil {
			err = openErr
		} else {
			err = dir.Sync()
			_ = dir.Close()
		}
	}
	if err != nil {
		return err
	}
	s.records = state.Watchers
	return nil
}

func (s *Service) load() error {
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxStateBytes || info.Mode().Perm()&0o077 != 0 {
		return errors.New("page watcher state must be an owner-only regular file within the size limit")
	}
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return err
	}
	var state diskState
	if len(data) > maxStateBytes || json.Unmarshal(data, &state) != nil || state.Version != 1 || len(state.Watchers) > maxWatchers {
		return errors.New("invalid page watcher state")
	}
	for id, r := range state.Watchers {
		opts, err := normalize(r.RegisterOptions)
		if err != nil || opts != r.RegisterOptions || id != r.ID || len(r.Events) > maxEvents {
			return errors.New("invalid persisted page watcher")
		}
		for i, e := range r.Events {
			if e.WatcherID != id || e.Seq > r.Seq || e.URL != r.URL || (e.Kind != "changed" && e.Kind != "unavailable" && e.Kind != "recovered") || (i > 0 && e.Seq != r.Events[i-1].Seq+1) {
				return errors.New("invalid persisted page watcher events")
			}
		}
		if r.Enabled {
			r.Status = "starting"
			r.LastError = ""
		} else {
			r.Status = "paused"
		}
		r.TabID = ""
		state.Watchers[id] = r
	}
	if state.Watchers != nil {
		s.records = state.Watchers
	}
	return nil
}
