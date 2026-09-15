package browser

import (
	"errors"
	"strings"
)

// RouteRedirect names the interception behaviour brw deliberately does NOT
// implement. It exists so the refusal can be spelled once and enumerated by a
// test, not because a redirect route can be installed: nothing constructs a
// Route with this behaviour, and both transports refuse it before a rule is
// built.
const RouteRedirect RouteBehaviour = "redirect"

// ErrRouteRedirectUnsupported is what brw_route answers for behaviour=redirect.
//
// declarativeNetRequest and the Fetch domain both offer a redirect, so the gap
// is a decision rather than a missing primitive, and the reason was measured on
// each transport rather than assumed:
//
//   - Extension bridge. A declarativeNetRequest redirect rule is ACCEPTED by
//     updateSessionRules and listed by getSessionRules, and then never applies,
//     because a redirect action needs host permissions for the request URL and
//     for its initiator. brw's extension holds host permissions for loopback
//     only, on purpose. Shipping the rule without that access is the worst
//     available outcome: it installs without error and does nothing, so an agent
//     believes a third-party origin is stubbed while the page reaches it for
//     real. Buying the access means host permissions for the sites the user
//     browses. Measured in
//     TestDeclarativeNetRequestRedirectNeverFiresUnderShippedPermissions, which
//     grants the two halves separately: a loopback request URL is not
//     redirected either when the page asking for it is off-permission, so the
//     initiator is a second requirement rather than a restatement of the first.
//   - Direct CDP. Fetch.continueRequest can rewrite the URL, but Chrome does
//     NOT pause the rewritten request again — only the server's own 30x hop
//     comes back through the interception. The destination would therefore
//     reach the network without passing containmentVerdict, which is the check
//     that makes the navigation policy a boundary rather than a filter, so the
//     one behaviour that widens what a page can reach would also be the one the
//     boundary cannot see. Measured in TestRewrittenRequestURLIsNotPausedAgain.
//
// The refusal is shared rather than written per transport so the gap cannot be
// closed on one side and left open on the other.
var ErrRouteRedirectUnsupported = errors.New("brw has no redirect behaviour on either transport: on the extension bridge a declarativeNetRequest redirect rule is accepted and listed but never applied, because it needs host permissions for the request URL AND its initiator, which for a real site means every site the user browses; on direct-CDP a URL rewritten with Fetch.continueRequest is not paused again, so the destination would reach the network without passing the containment boundary that gates every other request. Answer the request yourself with behaviour=fulfill, replay a recording with action=replay, or point the page at the other server")

// UnsupportedRouteBehaviours is the whole set of behaviours brw refuses by name
// rather than implements, keyed by the spelling a caller sends.
//
// It is a table for the reason every other gate here is: a refusal written at
// two call sites is a refusal one transport can drift away from. Both backends
// enumerate this map against their own route path, and the MCP catalogue
// enumerates it against the behaviours brw_route advertises, so a member that
// one transport starts accepting — or that the schema starts offering — fails
// the build rather than shipping as a half-transport capability.
var UnsupportedRouteBehaviours = map[RouteBehaviour]error{
	RouteRedirect: ErrRouteRedirectUnsupported,
}

// CheckRouteBehaviourSupported returns the named refusal for a behaviour brw
// deliberately does not implement, and nil for everything else — including an
// unknown word, which each backend still rejects with its own message naming the
// behaviours it does support.
func CheckRouteBehaviourSupported(raw string) error {
	return UnsupportedRouteBehaviours[RouteBehaviour(strings.ToLower(strings.TrimSpace(raw)))]
}
