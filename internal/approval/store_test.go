package approval

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "approvals.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, path
}

func request() Request {
	return Request{Fingerprint: "fingerprint", Tool: "brw_click", StateDigest: "state", Arguments: json.RawMessage(`{"selector":"#submit"}`)}
}

func enqueue(t *testing.T, s *Store) Request {
	t.Helper()
	r, err := s.Enqueue(request())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("got error %v, want code %q", err, code)
	}
}

func approve(t *testing.T, s *Store, id string) Request {
	t.Helper()
	r, err := s.Decide(id, Approved, "max", "allowed")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLifecycleSurvivesReopen(t *testing.T) {
	s, path := openTestStore(t)
	r := enqueue(t, s)
	if r.Status != Pending || len(r.ID) != 32 || r.ExpiresAt.Sub(r.CreatedAt) != defaultTTL {
		t.Fatalf("unexpected request: %+v", r)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loaded, ok := s.Get(r.ID)
	if !ok || loaded.Status != Pending {
		t.Fatalf("pending missing after reopen: %+v", loaded)
	}
	approved := approve(t, s, r.ID)
	if approved.DecisionBy != "max" || approved.DecidedAt.IsZero() {
		t.Fatalf("missing decision audit: %+v", approved)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	consumed, err := s.Consume(r.ID, r.Fingerprint, r.StateDigest)
	if err != nil || consumed.Status != Consumed {
		t.Fatalf("consume: %+v, %v", consumed, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.Consume(r.ID, r.Fingerprint, r.StateDigest)
	expectCode(t, err, Consumed)
	duplicate := enqueue(t, s)
	if duplicate.ID != r.ID || duplicate.Status != Consumed {
		t.Fatalf("consumed request requeued: %+v", duplicate)
	}
}

func TestConsumeExactlyOnce(t *testing.T) {
	s, _ := openTestStore(t)
	r := enqueue(t, s)
	approve(t, s, r.ID)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Consume(r.ID, r.Fingerprint, r.StateDigest); err == nil {
				successes.Add(1)
			} else {
				var coded *Error
				if !errors.As(err, &coded) || coded.Code != Consumed {
					t.Errorf("unexpected consume refusal: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful dispatch authorizations: %d", successes.Load())
	}
}

func TestDuplicateAndDeniedDecision(t *testing.T) {
	s, _ := openTestStore(t)
	r := enqueue(t, s)
	if again := enqueue(t, s); again.ID != r.ID {
		t.Fatal("pending duplicate created")
	}
	approve(t, s, r.ID)
	if again := enqueue(t, s); again.ID != r.ID {
		t.Fatal("approved duplicate created")
	}
	_, err := s.Decide(r.ID, Denied, "max", "reverse")
	expectCode(t, err, Approved)
	first, err := s.Decide(r.ID, Approved, "other", "changed")
	if err != nil || first.DecisionBy != "max" || first.DecisionNote != "allowed" {
		t.Fatalf("idempotent decision modified audit: %+v, %v", first, err)
	}
	different := request()
	different.Fingerprint = "different"
	denied, err := s.Enqueue(different)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(denied.ID, Denied, "max", "no"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Consume(denied.ID, denied.Fingerprint, denied.StateDigest)
	expectCode(t, err, Denied)
	_, err = s.Decide(denied.ID, Approved, "max", "reverse")
	expectCode(t, err, Denied)
	again, err := s.Enqueue(different)
	if err != nil || again.ID != denied.ID || again.Status != Denied {
		t.Fatalf("denied duplicate requeued: %+v, %v", again, err)
	}
}

func TestExpiryIsPersisted(t *testing.T) {
	for _, touch := range []string{"get", "list", "decide", "consume", "enqueue", "invalidate"} {
		t.Run(touch, func(t *testing.T) {
			s, path := openTestStore(t)
			now := time.Now().UTC()
			s.now = func() time.Time { return now }
			r := enqueue(t, s)
			approve(t, s, r.ID)
			now = r.ExpiresAt
			switch touch {
			case "get":
				s.Get(r.ID)
			case "list":
				s.List()
			case "decide":
				_, err := s.Decide(r.ID, Approved, "max", "late")
				expectCode(t, err, Expired)
			case "consume":
				_, err := s.Consume(r.ID, r.Fingerprint, r.StateDigest)
				expectCode(t, err, Expired)
			case "enqueue":
				fresh := enqueue(t, s)
				if fresh.ID == r.ID {
					t.Fatal("expired request reused")
				}
			case "invalidate":
				expectCode(t, s.Invalidate(r.ID, "late"), Expired)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got, ok := reopened.Get(r.ID)
			if !ok || got.Status != Expired {
				t.Fatalf("expiry not durable: %+v", got)
			}
		})
	}
}

func TestBindingsInvalidateApproval(t *testing.T) {
	for _, binding := range []string{"fingerprint", "state"} {
		t.Run(binding, func(t *testing.T) {
			s, _ := openTestStore(t)
			r := enqueue(t, s)
			approve(t, s, r.ID)
			fingerprint, state := r.Fingerprint, r.StateDigest
			if binding == "fingerprint" {
				fingerprint = "changed"
			} else {
				state = "changed"
			}
			stale, err := s.Consume(r.ID, fingerprint, state)
			expectCode(t, err, Stale)
			if stale.Status != Stale {
				t.Fatalf("bindings did not invalidate: %+v", stale)
			}
			_, err = s.Consume(r.ID, r.Fingerprint, r.StateDigest)
			expectCode(t, err, Stale)
			_, err = s.Decide(r.ID, Approved, "max", "again")
			expectCode(t, err, Stale)
			if fresh := enqueue(t, s); fresh.ID == r.ID {
				t.Fatal("stale request reused")
			}
		})
	}
}

func TestInvalidateAndClosedStore(t *testing.T) {
	s, _ := openTestStore(t)
	r := enqueue(t, s)
	if err := s.Invalidate(r.ID, "navigation changed"); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get(r.ID)
	if !ok || got.Status != Stale || got.DecisionNote != "navigation changed" {
		t.Fatalf("invalidated request: %+v", got)
	}
	if err := s.Invalidate(r.ID, "again"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Decide("missing", Approved, "max", "")
	expectCode(t, err, "not_found")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = s.Enqueue(request())
	expectCode(t, err, "store_closed")
	if _, ok := s.Get(r.ID); ok {
		t.Fatal("closed store exposed request")
	}
}

func TestDisplayCopiesAndTTLBounds(t *testing.T) {
	s, _ := openTestStore(t)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	input := request()
	input.ExpiresAt = now.Add(24 * time.Hour)
	r, err := s.Enqueue(input)
	if err != nil {
		t.Fatal(err)
	}
	if r.ExpiresAt.Sub(r.CreatedAt) != maxTTL {
		t.Fatal("TTL not capped")
	}
	input.Arguments[0] = 'x'
	r.Arguments[0] = 'y'
	got, _ := s.Get(r.ID)
	if !json.Valid(got.Arguments) {
		t.Fatal("Enqueue returned aliased arguments")
	}
	got.Arguments[0] = 'z'
	list := s.List()
	if !json.Valid(list[0].Arguments) {
		t.Fatal("Get returned aliased arguments")
	}
	list[0].Arguments[0] = 'q'
	got, _ = s.Get(r.ID)
	if !json.Valid(got.Arguments) {
		t.Fatal("List returned aliased arguments")
	}
	input = request()
	input.Fingerprint = "expired"
	input.ExpiresAt = now
	_, err = s.Enqueue(input)
	expectCode(t, err, "invalid_request")
}

func TestPersistenceFailureRollsBackAndFailsClosed(t *testing.T) {
	for _, operation := range []string{"enqueue", "decide", "consume", "expire"} {
		t.Run(operation, func(t *testing.T) {
			s, path := openTestStore(t)
			r := enqueue(t, s)
			if operation == "consume" {
				approve(t, s, r.ID)
			}
			before := s.requests[r.ID].Status
			s.save = func(map[string]Request) error { return errors.New("disk unavailable") }
			var err error
			switch operation {
			case "enqueue":
				input := request()
				input.Fingerprint = "new"
				_, err = s.Enqueue(input)
			case "decide":
				_, err = s.Decide(r.ID, Approved, "max", "")
			case "consume":
				_, err = s.Consume(r.ID, r.Fingerprint, r.StateDigest)
			case "expire":
				s.now = func() time.Time { return r.ExpiresAt }
				if s.List() != nil {
					t.Fatal("failed expiry exposed requests")
				}
				err = s.fault
			}
			expectCode(t, err, "persistence_failed")
			if s.requests[r.ID].Status != before || len(s.requests) != 1 {
				t.Fatal("failed persistence changed memory")
			}
			_, err = s.Consume(r.ID, r.Fingerprint, r.StateDigest)
			expectCode(t, err, "persistence_failed")
			if _, ok := s.Get(r.ID); ok {
				t.Fatal("poisoned store exposed approval")
			}
			s.Close()
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got, ok := reopened.Get(r.ID)
			if !ok || got.Status != before {
				t.Fatalf("failed write changed disk: %+v", got)
			}
		})
	}
}

func TestActualRenameFailureFailsClosed(t *testing.T) {
	s, path := openTestStore(t)
	r := enqueue(t, s)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	_, err := s.Decide(r.ID, Approved, "max", "")
	expectCode(t, err, "persistence_failed")
	if s.requests[r.ID].Status != Pending {
		t.Fatal("rename failure approved in memory")
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".approval-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files remain: %v, %v", leftovers, err)
	}
}

func TestCorruptionFailsClosedAndReleasesLock(t *testing.T) {
	for _, data := range []string{"", "{", `{"version":2,"requests":[]}`, `{"version":1,"requests":null}`, `{"version":1,"requests":[{}]}`} {
		t.Run(fmt.Sprintf("%q", data), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private", "approvals.json")
			if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := Open(path)
			expectCode(t, err, "store_corrupt")
			if err := os.WriteFile(path, []byte(`{"version":1,"requests":[]}`), 0600); err != nil {
				t.Fatal(err)
			}
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
		})
	}
}

func TestQueueBoundAndTerminalCleanup(t *testing.T) {
	s, _ := openTestStore(t)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	for i := 0; i < maxRequests; i++ {
		r := request()
		r.ID, r.Fingerprint = fmt.Sprintf("id-%d", i), fmt.Sprintf("fp-%d", i)
		r.Status, r.CreatedAt, r.ExpiresAt = Pending, now.Add(time.Duration(i)*time.Millisecond), now.Add(defaultTTL)
		s.requests[r.ID] = r
	}
	_, err := s.Enqueue(request())
	expectCode(t, err, "queue_full")
	old := s.requests["id-0"]
	old.Status = Consumed
	old.ExpiresAt = now
	s.requests[old.ID] = old
	newer := s.requests["id-1"]
	newer.Status = Denied
	newer.ExpiresAt = now
	s.requests[newer.ID] = newer
	enqueue(t, s)
	if len(s.List()) != maxRequests {
		t.Fatal("queue exceeded bound")
	}
	if _, ok := s.Get(old.ID); ok {
		t.Fatal("oldest terminal retained")
	}
	if _, ok := s.Get(newer.ID); !ok {
		t.Fatal("newer terminal removed")
	}
}

func TestTerminalEvictionBreaksCreationTimeTiesByID(t *testing.T) {
	s, _ := openTestStore(t)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	for i := 0; i < maxRequests; i++ {
		r := request()
		r.ID, r.Fingerprint = fmt.Sprintf("id-%04d", i), fmt.Sprintf("fp-%d", i)
		r.Status, r.CreatedAt, r.ExpiresAt = Expired, now.Add(-time.Hour), now.Add(-time.Minute)
		s.requests[r.ID] = r
	}
	enqueue(t, s)
	if _, found := s.Get("id-0000"); found {
		t.Fatal("terminal eviction did not remove the lowest ID for equal creation times")
	}
	if _, found := s.Get("id-0001"); !found {
		t.Fatal("terminal eviction removed more than one request")
	}
}

func TestPermissionsAndExclusiveLock(t *testing.T) {
	s, path := openTestStore(t)
	enqueue(t, s)
	for name, mode := range map[string]os.FileMode{path: 0600, path + ".lock": 0600, filepath.Dir(path): 0700} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s permission = %o, want %o", name, info.Mode().Perm(), mode)
		}
	}
	_, err := Open(path)
	expectCode(t, err, "store_locked")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessLockHelper$")
	cmd.Env = append(os.Environ(), "BRW_APPROVAL_LOCK_TEST="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child lock verification failed: %s, %v", output, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
}

func TestProcessLockHelper(t *testing.T) {
	path := os.Getenv("BRW_APPROVAL_LOCK_TEST")
	if path == "" {
		return
	}
	s, err := Open(path)
	if s != nil {
		s.Close()
	}
	expectCode(t, err, "store_locked")
}

func TestRejectInsecureFilesAndSymlinks(t *testing.T) {
	for _, kind := range []string{"directory", "file", "lock", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private", "approvals.json")
			if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "directory":
				if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(path, []byte(`{"version":1,"requests":[]}`), 0644); err != nil {
					t.Fatal(err)
				}
			case "lock":
				if err := os.WriteFile(path+".lock", nil, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(filepath.Dir(path), "target")
				if err := os.WriteFile(target, nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Open(path)
			expectCode(t, err, "invalid_request")
		})
	}
}

func TestReadAndDedupDoNotWrite(t *testing.T) {
	s, _ := openTestStore(t)
	r := enqueue(t, s)
	s.save = func(map[string]Request) error { t.Fatal("unchanged read or dedup attempted persistence"); return nil }
	s.Get(r.ID)
	s.List()
	again := enqueue(t, s)
	if again.ID != r.ID {
		t.Fatal("dedup changed request")
	}
}

func TestUnexpiredTerminalCannotBeEvicted(t *testing.T) {
	s, _ := openTestStore(t)
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	for i := 0; i < maxRequests; i++ {
		r := request()
		r.ID, r.Fingerprint = fmt.Sprintf("id-%d", i), fmt.Sprintf("fp-%d", i)
		r.Status, r.CreatedAt, r.ExpiresAt = Consumed, now, now.Add(defaultTTL)
		s.requests[r.ID] = r
	}
	_, err := s.Enqueue(request())
	expectCode(t, err, "queue_full")
	input := request()
	input.Fingerprint = "fp-0"
	r, err := s.Enqueue(input)
	if err != nil || r.Status != Consumed || r.ID != "id-0" {
		t.Fatalf("consumed disposition lost: %+v, %v", r, err)
	}
}

func TestFailureAfterDurableWriteNeverReplays(t *testing.T) {
	s, path := openTestStore(t)
	r := enqueue(t, s)
	approve(t, s, r.ID)
	s.save = func(requests map[string]Request) error {
		if err := s.persist(requests); err != nil {
			return err
		}
		return errors.New("failure after rename")
	}
	_, err := s.Consume(r.ID, r.Fingerprint, r.StateDigest)
	expectCode(t, err, "persistence_failed")
	if s.requests[r.ID].Status != Approved {
		t.Fatal("uncertain persistence did not roll back memory")
	}
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	_, err = reopened.Consume(r.ID, r.Fingerprint, r.StateDigest)
	expectCode(t, err, Consumed)
}

func TestConsumeSurvivesProcessExitWithoutClose(t *testing.T) {
	s, path := openTestStore(t)
	r := enqueue(t, s)
	approve(t, s, r.ID)
	s.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestConsumeExitHelper$")
	cmd.Env = append(os.Environ(), "BRW_APPROVAL_CONSUME_TEST="+path, "BRW_APPROVAL_CONSUME_ID="+r.ID)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("consume child failed: %s, %v", output, err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	_, err = reopened.Consume(r.ID, r.Fingerprint, r.StateDigest)
	expectCode(t, err, Consumed)
}

func TestConsumeExitHelper(t *testing.T) {
	path := os.Getenv("BRW_APPROVAL_CONSUME_TEST")
	if path == "" {
		return
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume(os.Getenv("BRW_APPROVAL_CONSUME_ID"), "fingerprint", "state"); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}
