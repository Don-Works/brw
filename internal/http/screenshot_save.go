package httpapi

import (
	"errors"
	"net/http"

	"github.com/Don-Works/brw/internal/browser"
)

func (s *Server) screenshotSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		browser.ScreenshotSaveOptions
		TabID string `json:"tab_id"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	saver, ok := s.manager.(browser.ScreenshotSaver)
	if !ok {
		writeError(w, errors.New("screenshot disk saving is unavailable on this transport; upgrade the browser host"))
		return
	}
	tabID := r.URL.Query().Get("tab_id")
	if tabID == "" {
		tabID = req.TabID
	}
	result, err := saver.SaveScreenshot(s.contextWithTabID(r.Context(), tabID), req.ScreenshotSaveOptions)
	writeResult(w, result, err)
}
