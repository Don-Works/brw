package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Don-Works/brw/internal/pagewatch"
)

// SetPageWatchAPI installs the browser-host service; proxies use their controller.
func (s *Server) SetPageWatchAPI(api pagewatch.API) { s.pageWatch = api }

func (s *Server) pageWatchService() pagewatch.API {
	if api, ok := s.manager.(pagewatch.API); ok {
		return api
	}
	return s.pageWatch
}

func (s *Server) pageWatchTool(ctx context.Context, name string, args json.RawMessage) (any, *rpcError) {
	api := s.pageWatchService()
	if api == nil {
		return toolError(errors.New("persistent page watchers are disabled; use a browser-host daemon with --page-watch-root auto")), nil
	}
	switch name {
	case "brw_watch_page":
		var req pagewatch.RegisterOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(api.WatchPage(ctx, req))
	case "brw_page_watchers":
		var req pagewatch.ManageOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(api.PageWatchers(ctx, req))
	default:
		var req pagewatch.EventsOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(api.PageEvents(ctx, req))
	}
}
