# Prior Art

This project is not greenfield by name or broad concept.

## Existing `agent-browser`

Vercel Labs already has an `agent-browser` repository and product. Its README describes it as a native Rust CLI for browser automation, with commands such as:

- `agent-browser snapshot` for accessibility tree refs.
- `agent-browser click @e2`.
- `agent-browser fill @e3`.
- `agent-browser connect <port>` for CDP.

That overlaps strongly with the product idea here. The reason to keep this repository is narrower:

- The requested implementation stack is Go with `chromedp` / `cdproto`.
- This project is daemon-first, with HTTP and MCP as stable surfaces.
- The long-term auth path needs to account for default-profile Chrome auth, which direct remote debugging no longer handles safely.

## Related Projects

- Microsoft Playwright MCP provides browser automation through structured accessibility snapshots instead of screenshot-only interaction.
- Chrome DevTools MCP exposes Chrome debugging and inspection capabilities through MCP.
- Several Chrome debug MCP servers attach to a manually launched Chrome debugging port.

## Product Position

The clean position is not "we invented semantic browser refs". The position is:

> A Go, daemon-first, harness-agnostic semantic browser control layer for a visible real Chrome/Chromium, designed around persistent user control and future installed-profile bridging.

If this becomes a public product, consider renaming or clearly scoping it to avoid confusion with Vercel's `agent-browser`.
