# Default-Profile Bridge Prototype

This directory is a placeholder for the installed Chrome extension bridge needed to control a user's already-authenticated default Chrome profile.

Direct CDP launch cannot carry the default profile on Chrome 136+. The bridge path is:

1. User installs an extension in their existing Chrome profile.
2. The extension requests the `debugger` permission.
3. The extension uses `chrome.debugger` as an alternate CDP transport for active tabs.
4. The extension connects to `agent-browserd` over localhost WebSocket or native messaging.
5. The Go daemon exposes the same HTTP/MCP tools, but routes browser operations through the extension transport.

This preserves existing cookies, passkeys, OAuth state, extensions, and financial-site sessions without extracting profile data.

The MVP daemon in this repository currently implements direct CDP launch/attach mode. The bridge should be implemented as an auditable second transport, not mixed into the direct `chromedp` manager.
