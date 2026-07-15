package httpapi

import (
	"fmt"
	"net/http"
	"time"

	"github.com/Don-Works/brw/internal/usagelog"
)

// usageOperations is intentionally an allowlist. Unknown/raw paths are never
// copied to the ledger because a caller could put secrets in a URL path.
var usageOperations = map[string]string{
	"/api/browser/open":              "brw_open",
	"/api/browser/open_incognito":    "brw_open_incognito",
	"/api/browser/close_context":     "brw_close_context",
	"/api/browser/tabs":              "brw_list_tabs",
	"/api/browser/tab_groups":        "brw_list_tab_groups",
	"/api/browser/focus":             "brw_focus_tab",
	"/api/browser/close":             "brw_close_tab",
	"/api/browser/emulate_device":    "brw_emulate_device",
	"/api/browser/group_tabs":        "brw_group_tabs",
	"/api/browser/ungroup_tabs":      "brw_ungroup_tabs",
	"/api/page/snapshot":             "brw_snapshot",
	"/api/page/find":                 "brw_find",
	"/api/page/read":                 "brw_read",
	"/api/page/read_data":            "brw_read_data",
	"/api/page/click":                "brw_click",
	"/api/page/click_text":           "brw_click_text",
	"/api/page/navigate":             "brw_navigate",
	"/api/page/navigate_to":          "brw_navigate_to",
	"/api/page/drag":                 "brw_drag",
	"/api/page/mouse_down":           "brw_mouse_down",
	"/api/page/mouse_up":             "brw_mouse_up",
	"/api/page/type":                 "brw_type",
	"/api/page/fill":                 "brw_fill",
	"/api/page/upload_file":          "brw_upload_file",
	"/api/page/select":               "brw_select",
	"/api/page/press":                "brw_press",
	"/api/page/scroll":               "brw_scroll",
	"/api/page/wait_for":             "brw_wait_for",
	"/api/page/hover":                "brw_hover",
	"/api/page/evaluate":             "brw_evaluate",
	"/api/page/network_requests":     "brw_network_requests",
	"/api/page/network_capture":      "brw_network_capture",
	"/api/page/replay_request":       "brw_replay_request",
	"/api/page/execute_plan":         "brw_plan",
	"/api/page/batch":                "brw_batch",
	"/api/page/cancel":               "brw_cancel",
	"/api/page/observe":              "brw_observe",
	"/api/page/commit":               "brw_commit",
	"/api/page/notify":               "brw_notify",
	"/api/page/assert_visible":       "brw_assert_visible",
	"/api/page/assert_hidden":        "brw_assert_hidden",
	"/api/page/assert_text":          "brw_assert_text",
	"/api/page/assert_value":         "brw_assert_value",
	"/api/page/click_xy":             "brw_click_xy",
	"/api/page/window_bounds":        "brw_window_bounds",
	"/api/page/console":              "brw_console_messages",
	"/api/page/downloads":            "brw_downloads",
	"/api/page/trace":                "brw_trace",
	"/api/page/clear_trace":          "brw_clear_trace",
	"/api/visual/screenshot":         "brw_screenshot",
	"/api/visual/screenshot_element": "brw_screenshot_element",
}

type usageResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *usageResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *usageResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

func (s *Server) usageMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		operation := usageOperations[r.URL.Path]
		if operation == "" || s.usage == nil {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		capture := &usageResponseWriter{ResponseWriter: w}
		next.ServeHTTP(capture, r)
		status := capture.status
		if status == 0 {
			status = http.StatusOK
		}
		outcome := "ok"
		errorClass := ""
		errorFingerprint := ""
		if status >= http.StatusBadRequest {
			outcome = "error"
			errorClass = usagelog.SafeID(capture.Header().Get(usagelog.HeaderErrorClass))
			if errorClass == "" {
				errorClass = fmt.Sprintf("http_%d", status)
			}
			errorFingerprint = usagelog.SafeFingerprint(capture.Header().Get(usagelog.HeaderErrorFingerprint))
		}
		_ = s.usage.Record(usagelog.Event{
			Layer: "http", Operation: operation, Outcome: outcome,
			DurationMS: time.Since(started).Milliseconds(), HTTPStatus: status,
			ErrorClass: errorClass, ErrorFingerprint: errorFingerprint,
			Retryable: usagelog.Retryable(errorClass),
			SessionID: r.Header.Get(usagelog.HeaderSessionID),
			RequestID: r.Header.Get(usagelog.HeaderRequestID),
			Client:    r.Header.Get(usagelog.HeaderClient),
		})
	})
}
