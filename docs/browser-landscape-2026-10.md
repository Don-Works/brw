# Browser automation landscape and planner/executor design

Checked 1 October 2026 against primary vendor documentation, maintained repositories and release/package records. This extends [the focused competitor review](competitive-review-2026-10.md) to infrastructure, extraction, computer use and durable orchestration. No cross-product performance benchmark was run. Capability statements describe documented surfaces; they do not establish reliability, relative speed, total cost or suitability for a particular signed-in workflow.

## Eight additional ecosystems

These products occupy different layers. Hosted browser infrastructure can supply a browser to an agent, extraction services can remove the need for a browser interaction, and durable orchestration can supervise brw. Counting all of them as interchangeable browser agents would obscure the useful choices.

| Ecosystem and layer | Verified current surface/version | Actual documented capabilities | Relationship to brw and opportunity |
| --- | --- | --- | --- |
| Browserbase: managed infrastructure | Current hosted Browser API; the service has no single verified public semver in the inspected docs. | Create/control/observe browser sessions; integrations with Playwright, Puppeteer and Selenium. Its platform also advertises search, markdown fetch, agent identity and hosted functions. [Browserbase docs](https://docs.browserbase.com/welcome/introduction). | Complementary infrastructure; overlaps browser attachment and session lifecycle. brw already has local/direct/bridge lanes. A remote-provider adapter would need explicit session ownership, expiry, cleanup and data-residency configuration; this report does not establish that every provider feature is usable through brw today. |
| Browserless: browser service and automation API | Open-source release [v2.56.7](https://github.com/browserless/browserless/releases/tag/v2.56.7); cloud BrowserQL/BAP are separately documented surfaces. | Puppeteer/Playwright browser connections; REST extraction/PDF/screenshots; GraphQL-style BrowserQL operations. Reconnect preserves a running session and can return a CDP endpoint; expired sessions still require lifecycle handling. [API choices](https://docs.browserless.io/), [reconnect](https://docs.browserless.io/browserql/session-management/reconnect-to-browserless). | Complementary hosted browser capacity and overlap in automation. Evaluate explicit remote session attachment if needed. BrowserQL itself is a different transport from CDP, so it is not automatically a drop-in backend. Avoid competing on unsupported claims about vendor anti-bot features. |
| Firecrawl: extraction plus interaction | Documented [v2 scrape API](https://docs.firecrawl.dev/features/scrape); hosted Interact/Browser Sandbox have their own session lifecycle. | Markdown and schema/prompt-based JSON extraction; after scraping, Interact accepts a prompt or Playwright code in the same page/session. Standalone sessions and persistent profiles are also documented. [Interact](https://docs.firecrawl.dev/features/interact). | Overlaps brw read_url/read_data and interaction; complementary for managed extraction workflows. brw's deterministic structured reads are already useful, while arbitrary model-assisted schema extraction is a separate adapter opportunity. Require evidence/source fields and invalid-output errors rather than silently inventing required values. |
| Crawl4AI: local crawl/extraction library | [v0.9.4](https://github.com/unclecode/crawl4ai/releases/tag/v0.9.4). | CSS/XPath and regex extraction without per-page LLM inference; generated extraction schemas can be validated and reused. Adaptive crawling has an explicit relevance/coverage stopping approach. [LLM-free extraction](https://docs.crawl4ai.com/extraction/no-llm-strategies/), [adaptive crawling](https://docs.crawl4ai.com/core/adaptive-crawling/). | Complementary crawl engine and overlap in deterministic extraction. brw already supports scoped page reads and recipes; it is not a multi-page relevance crawler. A bounded extraction recipe and multi-sample validation are more directly relevant than copying a whole crawler into brw. Crawl stopping policy belongs above the browser engine. |
| Anthropic browser/computer-use ecosystem: model-defined client tools | Current documented `browser_toolset_20260801` and `computer_toolset_20260801`. | Browser use combines accessibility structure, forms/tabs and screenshots/coordinates; computer use supplies desktop screenshot/mouse/keyboard tools. The application executes calls in its own environment. [Browser tool](https://platform.claude.com/docs/en/agents-and-tools/tool-use/browser-use-tool), [computer tool](https://platform.claude.com/docs/en/agents-and-tools/tool-use/computer-use-tool). | Competing agent-facing tool contract and possible complementary adapter. brw already offers semantic refs and screenshot fallback. A contract translator must preserve targeting, coordinate spaces and authority restrictions, rather than merely rename verbs. Desktop-wide execution belongs to a separate restricted environment; brw need not become a desktop controller. No model ranking follows from tool availability. |
| Skyvern: task/workflow automation | [v1.0.54](https://github.com/Skyvern-AI/skyvern/releases/tag/v1.0.54). | Task goals can carry output schemas and stop/error codes. Workflow blocks include browser tasks/actions, extraction, validation, loops, file parsing, HTTP and custom code; cloud browser/page extraction and live viewing are documented. [Maintained repository](https://github.com/Skyvern-AI/skyvern), [SDK quickstart](https://www.skyvern.com/docs/developers/getting-started/quickstart). | Overlaps recipes and observation; complements a thin executor with a broader workflow product. Adopt typed stop conditions and validation in the orchestrator. Email, scheduling, credential workflows and business integrations do not need to be embedded in brw. Vendor benchmark claims are not reproduced here. |
| LangGraph: agent orchestration | Python package [1.2.12](https://pypi.org/project/langgraph/1.2.12/). The repository's generic latest-release link points to a CLI development release, not necessarily the core package. | Graph checkpointers persist thread state for continuity, interruption and fault tolerance; stores persist cross-thread application data. In-memory checkpoints do not survive restart. [Persistence docs](https://docs.langchain.com/oss/python/langgraph/persistence). | Complementary orchestration, not another browser. Place planner/executor dispatch, escalation and checkpointing here or in Maix. A restored graph is not a restored browser document: renewed leases and fresh observations are still necessary. No framework adoption is required to use this design. |
| Temporal: durable workflow harness | Server release [v1.32.0](https://github.com/temporalio/temporal/releases/tag/v1.32.0); SDK and cloud versions are distinct. | Event-history replay reconstructs workflow state; replay-safe workflow code delegates outside-world calls, including LLM calls, to Activities. [Workflow/replay documentation](https://docs.temporal.io/workflows). | Complementary for long-lived operations, deadlines and recovery. Durable workflow history does not grant exactly-once browser writes. Put browser execution in bounded Activities with business-operation identities and reconciliation before retrying ambiguous external effects. brw stays a deterministic execution dependency. |

The existing agent-facing comparison also needs current identities pinned before evaluation: Playwright MCP [v0.0.83](https://github.com/microsoft/playwright-mcp/releases/tag/v0.0.83), Playwright CLI [v0.1.22](https://github.com/microsoft/playwright-cli/releases/tag/v0.1.22), and Browser Use [0.13.10](https://github.com/browser-use/browser-use/releases/tag/0.13.10). Microsoft's CLI documents skill-based operation and reduced schema/context exposure as its intended design; that is not a measured advantage over brw. [CLI source](https://github.com/microsoft/playwright-cli). Stagehand's current site includes [v4 documentation](https://docs.stagehand.dev/v4/first-steps/introduction), while GitHub's latest release link resolved to [3.7.3](https://github.com/browserbase/stagehand/releases/tag/@browserbasehq/stagehand@3.7.3) during this check. Do not treat a docs generation and a published package release as interchangeable.

## What brw already provides and what remains an opportunity

Existing brw capabilities include semantic refs, targeted Find, frontier/compact snapshots, batch assertions, caller-supplied/stored recipes, no-browser read_url, structured read_data, artifact handling, progressive tool discovery, browser/profile targeting and tab leases. The October candidate also implements bounded snapshot history. These should be evaluated as existing primitives, not proposed as new features.

Useful missing or separately scoped work:

1. **Orchestrator handoff contract:** a validated objective, authority envelope, finite execution budget and result schema. A smaller model can drive existing primitives without the daemon owning model providers or prompts.
2. **Evidence-backed extraction:** deterministic extraction recipes first, optional semantic schema adapter second. Reuse learned schemas across representative page variants, with missing/ambiguous data preserved as errors or explicit unknowns.
3. **Freshness enforcement:** a public opaque document-generation identity and guarded execution preconditions would help reject stale plans mechanically. The internal daemon epoch and numeric snapshot version are not a public document-identity contract. Until such a guard exists, the orchestrator must reobserve and validate immediately before acting; this does not eliminate every time-of-check/time-of-use race.
4. **Explicit remote-browser adapters:** only when workload evidence calls for managed capacity. Attach/session continuity, permission enforcement and resource cleanup need integration tests; vendor branding is not protocol compatibility.
5. **End-to-end evaluation:** include model handoff, retries, browser startup, observations, schema validation and outcome verification. Catalogue size alone cannot establish full-task savings.

[Algorithm experiments](algorithm-research-2026-10.md) remain distinct, unimplemented research proposals.

## Larger planner, smaller executor: an orchestrator design

This is a proposed architecture, not an implemented brw model router. Model size is not itself evidence of fitness. A tiny, mid-sized and larger executor should qualify on the same task cohort before routing. This report names no winning model or provider.

The planner chooses a narrow objective and the permitted strategy. The orchestrator validates the contract, acquires a tab lease, supplies only the needed tool definitions and compact evidence, then dispatches the executor. The executor runs a bounded observe–act–assert loop locally within that delegation, avoiding a frontier-model round trip for each routine field. The orchestrator enforces budgets and authority independently of model obedience. The planner sees outcomes and evidence when the unit completes or escalates.

Illustrative handoff fields are a proposed orchestrator schema, not brw tool arguments:

```json
{
  "objective_id": "read-order-42",
  "plan_epoch": 3,
  "objective": "Read the status of order 42",
  "tab_id": "owned-tab",
  "lease_owner": "executor-run",
  "expected_document": "orchestrator-observed-generation",
  "allowed_origins": ["https://fixture.test"],
  "allowed_actions": ["find", "observe", "read", "click"],
  "business_scope": {"order_id": "42", "read_only": true},
  "stop_before": ["purchase", "delete", "send"],
  "budget": {"actions": 12, "model_calls": 6, "seconds": 45},
  "success": {"order_id": "42", "status_present": true},
  "result_schema": "OrderStatusEvidenceV1"
}
```

Tool names alone do not enforce read-only business scope: a click can submit a destructive form. The gateway/orchestrator must restrict targets and operations, apply existing site permissions, and stop before actions outside the authorized objective. A worker must not gain arbitrary JavaScript, file, network or credential authority just because it is the cheaper lane. Secrets stay behind references/resolvers; only required values reach permitted fields.

Each delegation gets a disjoint leased tab. Never let planner and executor—or two executors—drive the same tab concurrently. Independent tabs still share cookies and can share application state such as a cart: use separate contexts/accounts or serialize the affected business operation. On handoff, the previous executor stops before the next acquires ownership. Expired leases, context replacement and navigation invalidate cached refs/plans; a checkpoint must reacquire state rather than resume a coordinate click from saved text.

Escalation signals should be typed: ambiguous target count; failed precondition; document/plan epoch drift; unrecognized modal; missing semantic coverage; unsupported frame action; authority boundary; ambiguous write result; repeated failure/no progress; or budget exhaustion. Return current URL, lease/epoch identity, relevant refs, assertion results, error codes and artifact IDs. Share a short operational outcome and requested decision, not chain-of-thought. A larger model's repair creates a new plan epoch and cannot quietly widen authority. Authentication or permission failures are not fixed by switching models.

Browser Use already exposes a separate page-extraction model, structured output validation and finite action/failure controls; that supports the general separation pattern without proving which model should execute brw actions. [Agent configuration](https://docs.browser-use.com/open-source/customize/agent/all-parameters).

## Amortize repeat work through recipes

Prefer a verified recipe when task shape and authority match. The learned path is: representative fixtures → planner-generated candidate → deterministic validation → versioned recipe → bounded execution with assertions → typed escalation on drift. Store semantic selectors/assertions and input schema, not live refs or screenshot coordinates. Validate business identity as well as DOM shape. Reuse does not authorize a repeated external write after an uncertain result.

A versioned immutable recipe can remove model inference from a stable inner loop entirely. Compare deterministic recipe plus planner against a small executor plus planner and a large model alone; inference-free execution is a separate baseline, not evidence that a smaller model won.

Measure average cost as `(learning + validation + all executions + repairs) / completed tasks`. Compare cold discovery with warm recipe runs separately. A failed recipe followed by two model repairs can cost more than the original direct large-model run; count that entire path. Crawl4AI's schema generation/reuse is a concrete extraction analogue, while brw already supplies recipe primitives.

## Evaluation matrix before choosing a small-model lane

Use fake local fixtures with deterministic reset and held-out variants. Pin exact model/version, decoding settings, tokenizer, tool catalogue, browser/build, transport and pacing. Reverify official availability/pricing at execution time; record actual provider usage rather than inventing model prices here.

| Mode | Purpose | Same-fixture comparisons |
| --- | --- | --- |
| Tiny executor, fixed large planner | Cheap bounded interpretation/execution hypothesis | Semantic reads, simple forms, repeated table extraction; normal and misleading near-match variants |
| Mid-sized executor, same planner | Quality/cost tradeoff | Multi-step navigation, delayed validation, menus, modal recovery, iframe limits |
| Large executor, same planner | Reference execution lane | Identical objectives, scopes, limits and evidence; no extra authority |
| Single large model end to end | Handoff overhead control | Same success criteria and allowed tools; include planning cost |
| Recipe-only execution; recipe plus each executor fallback | Amortization and repair | Cold learning, warm repeats and held-out UI drift |
| Disjoint-tab parallel execution | Throughput and isolation | Separate data/context fixtures; include lease conflicts and cancellation |

Measure verified success, wrong-target actions, unauthorized attempts/executions, duplicate writes, extraction errors, stale-plan rejection, retries/escalations, total input/output/image tokens, tool-response bytes, model/provider cost, browser cost, and full-task median/p95 wall time. Decompose latency into browser execution, model inference, tool/network latency, queueing, and repairs/handoffs; log browser startup and verification as well, then report the total. Smaller inference can lose its benefit through extra turns, tool calls or recovery. Amdahl's law gives an idealized bound: if inference occupies fraction `f` of end-to-end time and only inference becomes `s` times faster, speedup cannot exceed `1 / ((1-f) + f/s)` before new overhead. This is a bound for a fixed workflow, not a predicted measured gain. [Original Amdahl paper](https://doi.org/10.1145/1465482.1465560). Include failed and abandoned runs in cost accounting.

Context overhead also belongs in the experiment: the October candidate auto catalogue measured approximately 6.5k tokens using a characters/4 estimate, before a roughly 13 KiB skill document. Those are material inputs for tiny contexts; they are not tokenizer measurements. The gateway should load only schemas and concise goal/constraint/expected-evidence fields needed by the current objective, retaining an explicit discovery path when the next tool is unknown. Catalogue measurements are recorded in `/tmp/brw-catalogue.log`; they measure tool descriptions/schema payloads, not model execution performance.

Run paired randomized task orders with at least 30 independent runs per cohort, publish raw traces and uncertainty intervals, and predeclare success/error non-inferiority gates. An executor lane should qualify for a specific cohort rather than receive a universal “best model” label. These are proposed experiments; no results are asserted in this report.
