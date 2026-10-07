package mcp

import (
	"context"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

const pageSurfacesTimeout = 2 * time.Second

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

func (s *Server) openWithPageSurfaces(ctx context.Context, result browser.OpenResult, err error) (any, *rpcError) {
	if err != nil || result.NavigationErr() != nil {
		return openToolResult(result, err)
	}
	return toolJSON(openWithSurfaces{OpenResult: result, pageSurfaceFields: s.pageSurfaces(ctx)}, nil)
}

func (s *Server) navigationWithPageSurfaces(ctx context.Context, obs observer, result browser.ActionResult, err error) (any, *rpcError) {
	if err != nil || obs.level == browser.ObserveNone {
		return obs.navigation(result, err)
	}
	return toolJSON(navigationWithSurfaces{
		ActionResult:      obs.level.ApplyToNavigation(result),
		pageSurfaceFields: s.pageSurfaces(ctx),
	}, nil)
}
