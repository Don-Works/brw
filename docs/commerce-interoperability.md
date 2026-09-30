# WebMCP and commerce interoperability

brw discovers a site's agent surfaces before resorting to its human interface.
It can list and call page WebMCP tools through native or compatibility paths,
validate inputs, poll execution and signal cancellation. These are browser tool
contracts, not a universal checkout API or evidence that payment execution has
been validated. This work tested local discovery and browser-tool fixtures, not live purchases.

## WebMCP compatibility

WebMCP is a proposed standard; the inspected specification is a Draft Community
Group Report dated 30 September 2026. Its in-page API uses
`document.modelContext.getTools()` and `executeTool(tool, inputObject, options)`;
execution returns a stringified result. Chrome currently provides an origin
trial/development flag. Chrome 155 deprecates stringified inputs, Chrome 153
separates unregistering from cancellation of in-flight calls, and Chrome 156
introduces the debugging annotation and moves declarative lifecycle handlers
off the window surface. Implementations and editions continue to change.
[WebMCP draft](https://webmachinelearning.github.io/webmcp/),
[Chrome imperative API](https://developer.chrome.com/docs/ai/webmcp/imperative-api),
[Chrome declarative API](https://developer.chrome.com/docs/ai/webmcp/declarative-api).

Origin isolation and the tools Permissions Policy gate access. Cross-origin
exposure requires explicit policy and origin opt-in. brw's frame-target path
currently uses same-origin access; it does not promise every embedded payment
provider's tools are reachable. A page's consequential/read-only hint helps
classify a call, but cannot establish user authority or settlement.
[Chrome WebMCP overview](https://developer.chrome.com/docs/ai/webmcp/).

Native invocation chooses its input shape before execution: known Chromium
versions below 155 receive stringified inputs; modern or unrecognized runtimes
receive objects. A rejected call is not automatically repeated based on error
message text, because that text cannot prove execution never began.

Canceling a page-tool invocation delivers an AbortSignal and stops waiting. A
tool that ignores the signal can still complete its remote write. Navigation or
lost invocation state likewise cannot establish that no order was created.
Reconcile merchant state before repeating an ambiguous transaction.

## Read-only UCP profile discovery

`brw_read_url` reuses its existing `/.well-known/ucp` probe and retains the `ucp`
URL. A present response additionally yields an optional `ucp_profile` summary:
parse status, declared version, capability name/version rows, transport names,
payment handler name/ID rows, and a truncation indicator. Lists and strings are
bounded; malformed profiles report invalid, oversized profiles unavailable.
This parses the documented `ucp` envelope, not arbitrary older formats.

A parsed declaration is not a support or authorization verdict. Discovery does
not follow advertised schemas/endpoints, retrieve keys or tokens, invoke payment
handlers, negotiate a transaction or execute a purchase. UCP capability editions
and transports are separately declared; merchant support must be established
against a pinned contract. [UCP overview](https://ucp.dev/specification/overview/).

UCP checkout distinguishes incomplete, buyer escalation, ready-to-complete,
completion in progress, completed and canceled states. Buyer input/review can
require merchant `continue_url` handoff. Its REST and MCP bindings carry their
own metadata and idempotency rules. A page WebMCP tool named `complete_checkout`
is not automatically a remote UCP MCP operation.
[UCP checkout](https://ucp.dev/specification/shopping/checkout/),
[REST binding](https://ucp.dev/specification/shopping/checkout/rest/),
[MCP binding](https://ucp.dev/specification/shopping/checkout/mcp/).

## Payments and transaction evidence

Payment Request coordinates browser payment UI and payment-method responses.
The browser may reject missing user activation; `canMakePayment()` does not
prove a provisioned instrument, and abort may fail when another app owns the
interaction. Closing a sheet or calling `complete()` is not an independent
settlement receipt. [Payment Request API](https://www.w3.org/TR/payment-request/).

AP2 defines cryptographic transaction authorization through checkout/payment
mandates and their receipts. A brw consent prompt is not an AP2 mandate.
Its trusted surface, verifier, constraint validation and merchant/processor
integration require separately implemented and negotiated contracts; brw does
not claim a bundled AP2 wallet, signer or payment adapter.
[AP2 specification](https://ap2-protocol.org/ap2/specification/),
[AP2 authorization](https://ap2-protocol.org/ap2/agent_authorization/).

An invocation result, brw write receipt, merchant order confirmation, AP2
authorization receipt, processor payment receipt and fulfilled order answer
different questions. Recipe writes require declared postconditions and receipts
where applicable; an in-flight receipt prevents blind replay but cannot prove
settlement. Cancellation, voiding an authorization and refunding captured money
are separate operations. Protocol discovery alone grants none of them.

Future adapters should preserve transaction identity across timeout/navigation,
bind review to exact merchant/cart/amount/currency/expiry, suppress duplicate
writes, and reconcile authoritative order/payment state. These are integration
requirements, not purchase capabilities validated by the current discovery tests.
No single protocol here is established as universally implemented by merchants,
browsers or payment methods.
