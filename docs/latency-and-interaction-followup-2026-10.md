# Latency and interaction follow-up

The live workflow is the acceptance criterion. A successful transport reply,
an observed page change, or a fast benchmark does not establish that the user's
requested action took effect.

## Live workflow evidence

An authorized audit of a signed-in messaging setup on 2026-10-01 found 78 browser
calls across 43 execution groups over 517 seconds. The browser calls totalled
21,879 ms: 25 finds, 13 snapshots, 21 clicks, four fills, four reads, four waits,
and seven other operations. One wait failed after about six seconds. The 42
available code-execution envelopes totalled 70,783 ms, including browser work;
their median start-to-start interval was 12 seconds. These layers overlap and
must not be added. The time between requests is not attributed to model
inference without a host trace.

The user reported that Add was acknowledged but ineffective, and completed it
manually. The same target was clicked twice while the dialog remained open.
`changed_state:true` did not establish the requested outcome. The operator also
reported a missed styled checkbox, dialog controls buried under background
controls, and a focus change. No subsequent membership action was replayed.
Names, contact details, channel identifiers and page content are excluded here.

The resulting regression fixture places a dialog over 100 background controls,
keeps focus in the background, and includes a transparent native checkbox with
a visible label and an ARIA checkbox. The old default frontier omits the useful
dialog controls. The corrected frontier exposes them, preserves checkbox state,
and carries task scope into action observations. The test resolves the returned
refs, delivers real browser input, and checks the final member-added state.
This is a local regression, not a claim that the completed external workflow
was rerun successfully.

The click lane reproduced two independent acknowledgement/effect mismatches:
synthetic page events are not trusted browser events, and Chrome can acknowledge
mouse input to an inactive target without delivering it. Ref and text clicks
now validate the painted target and use trusted input. Coordinate clicks validate
the painted point before and after mouse movement. The signed-in extension lane
refuses an inactive target with an explicit focus remedy. An active tab in an
unfocused window remains usable: the headed fixture verified trusted delivery
without raising that window. Ambiguous press/release acknowledgements must not
replay input automatically.

A local dialog whose handler requires a trusted event now reaches exactly one
member-added result and closes on both direct and extension control. Disabled,
inert and occluded controls are negative cases, including an overlay introduced
by mouse movement. A separate foreground sentinel checks focus isolation.
These checks establish the reproduced input-delivery behavior; an application
can still reject valid input, so its own final state must be verified.

The expanded fixtures cover direct and extension control in both headed and
headless Chromium. They also cover nested disabled controls, disabled label
targets, open and captured closed shadow roots, HTTP pages without
`crypto.randomUUID`, and a tall text target with automatic scrolling disabled.
The old synthetic path produced one untrusted event and no business effect in
the controlled reproduction. Ref and text identity checks are stronger than
literal coordinates: a coordinate action still means the currently actionable
point, not a promise about a previously observed semantic element.

## Passive-step pacing

Chrome-profile daemons default to human pacing, while direct daemons default to
pacing off. Batches and plans previously charged that delay to passive steps
such as waits, snapshots and assertions. Passive steps now preserve the clock
of the last real UI action. Unknown actions and evaluations remain paced.

| Controlled measurement | n per arm | Before p50 / p95 ms | After p50 / p95 ms |
|---|---:|---:|---:|
| Human-paced 40 ms failing wait batch | 10 | 512.582 / 1548.919 | 41.955 / 45.681 |
| Pacing-off equivalent | 10 | 42.673 / 44.628 | 42.195 / 43.709 |
| Human-paced six-step direct flow | 8 | 4140.662 / 5016.929 | 1109.578 / 1552.155 |
| Human-paced six-step extension flow | 8 | 4379.569 / 5044.109 | 726.318 / 1355.722 |

The six-step flow fills, waits and asserts twice. Every run ended with the
expected value and exactly two input events. Real-action spacing stayed above
the configured human minimum. These Chromium 152 fixture measurements exclude
navigation, setup, model inference and gateway startup; the pacing samples are
small and unseeded. p95 is nearest rank, therefore the maximum at these sizes.

The extension wait also now has an absolute host deadline. A baseline 40 ms
wait against a hung renderer took about 2.14 seconds and a late positive reply
could be accepted after its deadline. Deterministic delayed-renderer tests now
reject late success, stop the following batch action, and clean up keyed
observers. Cleanup is asynchronous and bounded; an unusable renderer can still
require its own timer cleanup. Natural background throttling was not reproduced.

## Bounded tab discovery

`brw_list_tabs({format:"compact"})` returns at most 40 rows by default. Optional
literal query, ownership and limit filters preserve full tab identities and
explicitly report truncation and unknown ownership. The legacy default array
is unchanged. The usage log records output format, actual response bytes and
compact truncation without retaining query text.

For 1,000 synthetic tabs, serialized MCP output measured 456,012 bytes for the
legacy result, 219,624 bytes for all 1,000 compact rows, and 8,960 bytes for the
default 40-row compact response. That last reduction is 98.0%, with omitted
matches explicitly reported. This projection does not reduce upstream browser
enumeration. Actual provider context and model-visible savings remain separate
measurements.

Reuse the tab ID returned by open, inspect the preceding action's observation,
and batch known steps with a specific final-state assertion. Repeated discovery
and a generic click acknowledgement are not substitutes for task completion.
