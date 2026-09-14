package recipe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func compiledDraft(t *testing.T) Draft {
	t.Helper()
	return NewDraft(mustCompile(t, sixStepLoggedInTrace(), compileOptions()), "brw_trace")
}

// providerStub is the private provider's write API as far as this repository
// can know it: a server that answers the documented paths. It counts requests
// so a test can prove a refusal happened before anything left the machine.
type providerStub struct {
	requests int
	bodies   map[string]json.RawMessage
	handler  func(path string, body []byte) (int, string)
}

func newProviderStub(t *testing.T, handler func(path string, body []byte) (int, string)) (*providerStub, *HTTPProvider) {
	t.Helper()
	stub := &providerStub{bodies: map[string]json.RawMessage{}, handler: handler}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		stub.requests++
		stub.bodies[request.URL.Path] = json.RawMessage(body)
		status, reply := stub.handler(request.URL.Path, body)
		writer.Header().Set("content-type", "application/json")
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, reply)
	}))
	t.Cleanup(server.Close)
	provider, err := NewHTTPProvider(HTTPProviderConfig{BaseURL: server.URL, RequestTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return stub, provider
}

func TestPublishDraftSendsTheReviewedDraftAndChecksTheAcknowledgement(t *testing.T) {
	draft := compiledDraft(t)
	digest, err := Digest(draft.Recipe)
	if err != nil {
		t.Fatal(err)
	}
	acknowledgement := func(id, version, sentDigest string) string {
		body, _ := json.Marshal(map[string]any{"draft": map[string]string{
			"id": id, "version": version, "digest": sentDigest,
			"review_url": "https://provider.example.test/drafts/1",
		}})
		return string(body)
	}

	t.Run("published", func(t *testing.T) {
		stub, provider := newProviderStub(t, func(string, []byte) (int, string) {
			return http.StatusOK, acknowledgement(draft.Recipe.ID, draft.Recipe.Version, digest)
		})
		published, err := provider.PublishDraft(context.Background(), draft)
		if err != nil {
			t.Fatal(err)
		}
		if published.ID != draft.Recipe.ID || published.Digest != digest {
			t.Fatalf("published = %+v", published)
		}
		sent := stub.bodies["/v1/recipes/drafts"]
		var received Draft
		if err := json.Unmarshal(sent, &received); err != nil {
			t.Fatal(err)
		}
		if received.Review != draft.Review || received.Source != "brw_trace" {
			t.Fatalf("the provider did not receive the reviewed draft: %+v", received)
		}
		if len(received.Recipe.Steps) != len(draft.Recipe.Steps) {
			t.Fatalf("the provider received %d steps, want %d", len(received.Recipe.Steps), len(draft.Recipe.Steps))
		}
	})

	t.Run("acknowledgement names another recipe", func(t *testing.T) {
		_, provider := newProviderStub(t, func(string, []byte) (int, string) {
			return http.StatusOK, acknowledgement("example.other.recipe", draft.Recipe.Version, digest)
		})
		if _, err := provider.PublishDraft(context.Background(), draft); err == nil ||
			!strings.Contains(err.Error(), "does not match the one published") {
			t.Fatalf("err = %v, want the substitution refused", err)
		}
	})

	t.Run("acknowledgement names another digest", func(t *testing.T) {
		_, provider := newProviderStub(t, func(string, []byte) (int, string) {
			return http.StatusOK, acknowledgement(draft.Recipe.ID, draft.Recipe.Version,
				"0000000000000000000000000000000000000000000000000000000000000000")
		})
		if _, err := provider.PublishDraft(context.Background(), draft); err == nil ||
			!strings.Contains(err.Error(), "does not match the one published") {
			t.Fatalf("err = %v, want the substitution refused", err)
		}
	})
}

func TestPublishDraftRefusesBeforeAnythingLeavesTheMachine(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Draft)
		want   string
	}{
		{"no review body", func(d *Draft) { d.Review = "" }, "review body"},
		{"no source", func(d *Draft) { d.Source = "" }, "compiled from"},
		{"invalid recipe", func(d *Draft) { d.Recipe.Origins = nil }, "origins"},
		{"unverified write", func(d *Draft) {
			d.Recipe.Risk = "external_write"
			d.Recipe.Steps = append(d.Recipe.Steps, Step{
				ID: "send", Action: "click", Effect: "external_write",
				Target:         &Target{Role: "button", Name: "Request export"},
				IdempotencyKey: "send-the-export-request",
				Postcondition:  &Event{Kind: "text.present", Match: "Export requested", TimeoutMS: 1000},
			})
		}, "no assert step after it"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub, provider := newProviderStub(t, func(string, []byte) (int, string) {
				return http.StatusOK, `{"draft":{"id":"a.b.c","version":"1.0.0","digest":""}}`
			})
			draft := compiledDraft(t)
			test.mutate(&draft)
			if _, err := provider.PublishDraft(context.Background(), draft); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want it to mention %q", err, test.want)
			}
			if stub.requests != 0 {
				t.Fatalf("an invalid draft reached the network in %d request(s)", stub.requests)
			}
		})
	}
}

func receiptFixture(key string) Receipt {
	return Receipt{
		Key: key, RecipeID: "example.billing.download-invoices", RecipeVersion: "1.2.3",
		RecipeDigest: strings.Repeat("ab", 32), StepID: "send", Origin: receiptOrigin,
		Status: ReceiptInFlight,
	}
}

func TestReceiptClientRefusesAProviderAnsweringAboutAnotherWrite(t *testing.T) {
	key := strings.Repeat("0123456789abcdef", 4)
	reply := func(receipt Receipt, found bool) string {
		body, _ := json.Marshal(map[string]any{"found": found, "receipt": receipt})
		return string(body)
	}

	t.Run("lookup returns a receipt for another key", func(t *testing.T) {
		_, provider := newProviderStub(t, func(string, []byte) (int, string) {
			return http.StatusOK, reply(receiptFixture(strings.Repeat("f", 64)), true)
		})
		if _, _, err := provider.Lookup(context.Background(), key); err == nil ||
			!strings.Contains(err.Error(), "different key") {
			t.Fatalf("err = %v, want the mismatched key refused", err)
		}
	})

	t.Run("lookup finds nothing", func(t *testing.T) {
		_, provider := newProviderStub(t, func(string, []byte) (int, string) {
			return http.StatusOK, reply(Receipt{}, false)
		})
		record, found, err := provider.Lookup(context.Background(), key)
		if err != nil || found || record.Key != "" {
			t.Fatalf("record=%+v found=%v err=%v", record, found, err)
		}
	})

	t.Run("begin acknowledges another step", func(t *testing.T) {
		_, provider := newProviderStub(t, func(string, []byte) (int, string) {
			other := receiptFixture(key)
			other.StepID = "somebody-elses-write"
			body, _ := json.Marshal(map[string]any{"receipt": other})
			return http.StatusOK, string(body)
		})
		if _, err := provider.Begin(context.Background(), receiptFixture(key)); err == nil ||
			!strings.Contains(err.Error(), "different write") {
			t.Fatalf("err = %v, want the substitution refused", err)
		}
	})

	t.Run("commit reports it is still in flight", func(t *testing.T) {
		_, provider := newProviderStub(t, func(string, []byte) (int, string) {
			body, _ := json.Marshal(map[string]any{"receipt": receiptFixture(key)})
			return http.StatusOK, string(body)
		})
		if _, err := provider.Commit(context.Background(), key, "postcondition passed"); err == nil ||
			!strings.Contains(err.Error(), "reports status") {
			t.Fatalf("err = %v, want the unconfirmed commit refused", err)
		}
	})

	t.Run("commit is acknowledged", func(t *testing.T) {
		stub, provider := newProviderStub(t, func(string, []byte) (int, string) {
			committed := receiptFixture(key)
			committed.Status = ReceiptCommitted
			committed.Evidence = "postcondition passed"
			body, _ := json.Marshal(map[string]any{"receipt": committed})
			return http.StatusOK, string(body)
		})
		record, err := provider.Commit(context.Background(), key, "postcondition passed")
		if err != nil || record.Status != ReceiptCommitted {
			t.Fatalf("record=%+v err=%v", record, err)
		}
		if !strings.Contains(string(stub.bodies["/v1/receipts/commit"]), key) {
			t.Fatalf("commit request did not carry the key: %s", stub.bodies["/v1/receipts/commit"])
		}
	})

	t.Run("a malformed key never reaches the network", func(t *testing.T) {
		stub, provider := newProviderStub(t, func(string, []byte) (int, string) {
			return http.StatusOK, `{}`
		})
		if _, _, err := provider.Lookup(context.Background(), "not-a-digest"); err == nil {
			t.Fatal("a malformed key was accepted")
		}
		if stub.requests != 0 {
			t.Fatalf("a malformed key reached the network in %d request(s)", stub.requests)
		}
	})
}
