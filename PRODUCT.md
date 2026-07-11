# Product

## Register

product

## Users

Technical operators and developers who connect an AI-agent harness to a visible,
installed Chrome or Chromium profile. In the extension UI they are usually doing
one of two jobs: binding this browser profile to the correct local `brwd` daemon,
or diagnosing why that bridge is not connected.

## Product Purpose

brw gives agents fast, semantic control of a real browser while keeping the human
in charge of authentication, sensitive decisions, and visible takeover. The
extension is the trusted browser-side transport: it must make profile identity,
connection state, configuration, and failures obvious without exposing cookies,
passwords, passkeys, or bulk site storage. Success means an operator can configure
the right bridge once, verify it at a glance, and then forget the UI exists.

## Brand Personality

Direct, trustworthy, and technically precise. The product should feel calm under
failure, candid about limitations, and fast enough that its interface disappears
into the operator's task.

## Anti-references

- Marketing-dashboard ornament inside a small configuration utility.
- Security theatre: green states that are not backed by a live daemon response,
  hidden errors, or claims that overstate what the browser boundary guarantees.
- Novel controls, excessive rounding, decorative animation, or low-contrast muted
  copy that slows down an experienced operator.
- Raw diagnostic dumps as the only explanation of connection state.

## Design Principles

1. Show verified state, never inferred reassurance.
2. Make the safe and common configuration the shortest path.
3. Pair technical detail with an actionable human explanation.
4. Keep advanced identity fields available without making first setup feel dense.
5. Preserve operator control: never raise the browser's OS window, always restore
   transient tab activation used by an explicit screenshot, and never
   automatically confirm sensitive decisions.

## Accessibility & Inclusion

Target WCAG 2.2 AA. Preserve native keyboard behavior and visible focus, support
the browser's light/dark color scheme, never encode status by color alone, keep
body and placeholder text at readable contrast, and honor reduced-motion
preferences for every transition.
