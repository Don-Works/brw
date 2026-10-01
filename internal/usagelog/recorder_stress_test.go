package usagelog

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRecorderRecoversAfterRotationFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	r, err := New(Config{Path: path, MaxBytes: 1, Backups: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	event := Event{Layer: "mcp", Operation: "brw_read", Outcome: "ok"}
	if err := r.Record(event); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".1", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path+".1", "blocker"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.Record(event); err == nil {
		t.Fatal("expected rotation failure")
	}
	if err := os.RemoveAll(path + ".1"); err != nil {
		t.Fatal(err)
	}
	if err := r.Record(event); err != nil {
		t.Fatalf("recorder did not recover after obstruction removed: %v", err)
	}
}

func TestRecorderConcurrentRotationAndClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	r, err := New(Config{Path: path, MaxBytes: 4096, Backups: 3})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	ready := make(chan struct{}, 8)
	resume := make(chan struct{})
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 200; i++ {
				if i == 100 {
					ready <- struct{}{}
					<-resume
				}
				if err := r.Record(Event{Layer: "mcp", Operation: "brw_snapshot", Outcome: "ok", InputBytes: Count(0)}); err != nil && !errors.Is(err, os.ErrClosed) {
					t.Errorf("record: %v", err)
				}
			}
		}()
	}
	close(start)
	for worker := 0; worker < 8; worker++ {
		<-ready
	}
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	close(resume)
	wg.Wait()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Record(Event{}); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("record after close: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 4 {
		t.Fatalf("retention count=%d err=%v", len(entries), err)
	}
	for _, entry := range entries {
		file, err := os.Open(filepath.Join(filepath.Dir(path), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			var event Event
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				t.Errorf("partial rotated record: %v", err)
			}
		}
		if err := scanner.Err(); err != nil {
			t.Error(err)
		}
		file.Close()
	}
}
