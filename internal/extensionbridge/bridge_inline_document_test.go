package extensionbridge

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

func messagesOf(fake *navigationFakeExtension) []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]string(nil), fake.messages...)
}

func TestNavigateToFromTabWithoutCommittedDocument(t *testing.T) {
	const target = "https://api.test/data.json"
	b, fake, cleanup := newNavigationFake(t, target, "https://api.test", 20*time.Millisecond, false)
	defer cleanup()
	fake.mu.Lock()
	fake.uncommitted = true
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 3*time.Second)
	defer cancel()

	if err := b.navigateToURLAndWait(ctx, target); err != nil {
		t.Fatalf("navigate_to from an uncommitted tab: %v", err)
	}
	fake.mu.Lock()
	replaced, calls := fake.replaced, fake.navigateCalls
	fake.mu.Unlock()
	if !replaced || calls != 1 {
		t.Fatalf("replaced=%t navigate_calls=%d, want the one navigation to commit", replaced, calls)
	}
}

func TestNavigateToStillRefusesAnUnidentifiableCommittedDocument(t *testing.T) {
	b, fake, cleanup := newNavigationFake(t, "https://final.test/x", "https://final.test", 0, false)
	defer cleanup()
	fake.mu.Lock()
	fake.documentID = ""
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 2*time.Second)
	defer cancel()
	err := b.navigateToURLAndWait(ctx, "https://final.test/x")
	if err == nil || !strings.Contains(err.Error(), "pre-arm main-document identity") {
		t.Fatalf("navigate_to with an unidentifiable committed document = %v, want the pre-arm refusal", err)
	}
}

func TestNavigateToArmsInlineDocumentAroundPageNavigate(t *testing.T) {
	const target = "https://api.test/data.json"
	b, fake, cleanup := newNavigationFake(t, target, "https://api.test", 10*time.Millisecond, false)
	defer cleanup()
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 3*time.Second)
	defer cancel()
	if err := b.navigateToURLAndWait(ctx, target); err != nil {
		t.Fatalf("navigate_to: %v", err)
	}
	messages := messagesOf(fake)
	arm, navigate, disarm := slices.Index(messages, "arm_inline_document"), slices.Index(messages, "Page.navigate"), slices.Index(messages, "disarm_inline_document")
	if arm == -1 || navigate == -1 || disarm == -1 {
		t.Fatalf("messages = %v, want arm, Page.navigate and disarm", messages)
	}
	if !(arm < navigate && navigate < disarm) {
		t.Fatalf("messages = %v, want arm before Page.navigate and disarm after it", messages)
	}

	fake.mu.Lock()
	armURL := fake.armURL
	fake.mu.Unlock()
	if armURL != target {
		t.Fatalf("arm_inline_document url = %q, want the destination %q", armURL, target)
	}
}

func TestNavigateToWorksWithAnExtensionThatCannotArmInlineDocuments(t *testing.T) {
	const target = "https://api.test/data.json"
	b, fake, cleanup := newNavigationFake(t, target, "https://api.test", 10*time.Millisecond, false)
	defer cleanup()
	fake.mu.Lock()
	fake.rejectInlineArm = true
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 3*time.Second)
	defer cancel()
	if err := b.navigateToURLAndWait(ctx, target); err != nil {
		t.Fatalf("navigate_to with an old extension: %v", err)
	}
	if messages := messagesOf(fake); slices.Index(messages, "disarm_inline_document") != -1 {
		t.Fatalf("messages = %v, want no disarm after a refused arm", messages)
	}
}

func TestNavigateToNamesADownloadShapedAbort(t *testing.T) {
	b, fake, cleanup := newNavigationFake(t, "https://files.test/blob.bin", "https://files.test", 0, false)
	defer cleanup()
	fake.mu.Lock()
	fake.abortNavigate = true
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(browser.WithTabID(context.Background(), "42"), 2*time.Second)
	defer cancel()
	err := b.navigateToURLAndWait(ctx, "https://files.test/blob.bin")
	if err == nil || !strings.Contains(err.Error(), "served as a download") {
		t.Fatalf("navigate_to error = %v, want it to say the destination was a download", err)
	}
	if messages := messagesOf(fake); slices.Index(messages, "disarm_inline_document") == -1 {
		t.Fatalf("messages = %v, want the arm released after the failed navigation", messages)
	}
}
