package mcp

import (
	"context"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// pageSurfacesTimeout bounds the one evaluate that reads a page's agent
// surfaces after a navigation. It runs after the work the agent asked for, so a
// page that stalls it costs the hint, never the navigation result.
const pageSurfacesTimeout = 2 * time.Second

// pageSurfaceFields rides along on a navigation result: the page tools the new
// document registered and the agent endpoints it declares. Both are omitted
// when the page offers neither, so an ordinary page's result is unchanged.
type pageSurfaceFields struct {
	PageTools      []snapshot.PageToolSummary `json:"page_tools,omitempty"`
	PageToolsTotal int                        `json:"page_tools_total,omitempty"`
	AgentSurfaces  *snapshot.PageSurfaces     `json:"agent_surfaces,omitempty"`
}

type openWithSurfaces struct {
	browser.OpenResult
	pageSurfaceFields
}

type navigationWithSurfaces struct {
	browser.ActionResult
	pageSurfaceFields
}

// pageSurfaces reads the landed document's agent surfaces in one evaluate. Any
// failure yields no fields: the hint is best-effort and must never turn a
// navigation that worked into an error.
func (s *Server) pageSurfaces(ctx context.Context) pageSurfaceFields {
	evalCtx, cancel := context.WithTimeout(ctx, pageSurfacesTimeout)
	defer cancel()
	digest, err := snapshot.ReadPageSurfaces(evalCtx, s.pageToolEvaluator("surfaces"))
	if err != nil {
		return pageSurfaceFields{}
	}
	fields := pageSurfaceFields{PageTools: digest.Tools, PageToolsTotal: digest.ToolsTotal}
	if !digest.Surfaces.Empty() {
		surfaces := digest.Surfaces
		fields.AgentSurfaces = &surfaces
	}
	return fields
}

// openWithPageSurfaces is openToolResult for a navigation that landed: a failed
// one is reported exactly as before, with no page to read surfaces from.
func (s *Server) openWithPageSurfaces(ctx context.Context, result browser.OpenResult, err error) (any, *rpcError) {
	if err != nil || result.NavigationErr() != nil {
		return openToolResult(result, err)
	}
	return toolJSON(openWithSurfaces{OpenResult: result, pageSurfaceFields: s.pageSurfaces(ctx)}, nil)
}

// navigationWithPageSurfaces is observer.navigation plus the landed page's
// surfaces. observe:"none" asks for nothing beyond the outcome, so it skips the
// read entirely.
func (s *Server) navigationWithPageSurfaces(ctx context.Context, obs observer, result browser.ActionResult, err error) (any, *rpcError) {
	if err != nil || obs.level == browser.ObserveNone {
		return obs.navigation(result, err)
	}
	return toolJSON(navigationWithSurfaces{
		ActionResult:      obs.level.ApplyToNavigation(result),
		pageSurfaceFields: s.pageSurfaces(ctx),
	}, nil)
}
