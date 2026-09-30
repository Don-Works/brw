# Browser lanes and transport capabilities

Read before using cookies, incognito, session state, downloads or remote profiles.

## Headless or the signed-in browser: you choose

A machine can run a headless profile beside the signed-in ones. `brw_identity`
reports it as `headless: true` on `direct-cdp`; `brwctl setup --transport
headless` creates one (MCP server `brw-headless`, workspace
`brw-<browser>-headless`, user data in `~/.brw/<browser>-headless`). Its daemon
keeps the browser running between calls, so picking it costs no browser launch,
and it never opens a window or touches a human's profile.

Pick the lane per request. Do not ask the user when the table decides it:

| The request | Lane |
|---|---|
| Read a public page's text | `brw_read_url` on any profile; it opens no tab |
| Render, screenshot, click through or fill a form on a public site; call a public site's WebMCP tools | headless |
| Anything behind the user's login, their cookies or their open tabs | the signed-in profile the user means |
| The user wants to watch, take over, or solve a CAPTCHA | a windowed profile |

- The headless profile is never signed in, and you do not sign it in. A
  `fallback_hint` of `login_wall` or `auth_required`, or a login form where
  content was expected, means switch to the signed-in profile.
- Some sites refuse headless Chrome: a `challenge` hint, a CAPTCHA, or an empty
  403. Switch to a windowed profile instead of retrying.
- The signed-in lanes pace actions like a person by default: a gap of up to
  2.5s between actions and typing at key speed. Batch the flow rather than
  expecting instant steps; `brw_identity` does not change because of it.
- Headless is `direct-cdp`, so incognito contexts, `brw_cookies`, `brw_state`
  and download paths work there and tab groups do not.
- With no headless profile available, use the profile you have and carry on;
  mention `brwctl setup --transport headless` once in your reply if public work
  had to open the user's browser.
- A separate agent identity with its own logins is a brw-owned direct-CDP
  profile: `brwctl profiles create <name>` makes one, and `brwctl profiles copy
  --from A --to B --domain example.com` copies one site's cookies between two
  such profiles. It refuses the user's own browser at either end; that is an
  operator step, not a way around a login wall.

## Transport decides capabilities

| | `extension-bridge` | `direct-cdp` | `chrome-opt-in-cdp` | `remote-cdp` | `off-host-cdp` |
|---|---|---|---|---|---|
| drives | the human's existing signed-in Chrome, via the brw extension | a Chrome brw launched itself, often headless | the human's existing signed-in Chrome, with remote debugging switched on by hand at `chrome://inspect` | a browser another process started on this machine, attached with `--remote` | a browser a `browser.provider` plugin lent brw, on another machine |
| `brw_open_incognito` / `brw_close_context` | error: *"incognito browser contexts are not supported on the extension-bridge transport"* | works; `tab.context_id` comes back on open | works | works | works |
| `brw_cookies` | error: *"cookie access is not supported on the extension-bridge transport"* | works, including HttpOnly | works, including HttpOnly | works, including HttpOnly | works, including HttpOnly |
| `brw_state` | not advertised; calling it anyway errors: *"session snapshots are not supported on the extension-bridge transport"* | works | not advertised; calling it anyway errors: *"session snapshots are refused on a transport that drives the browser you are signed into"* | works | not advertised; all four actions error *"local session state is unavailable on a plugin-supplied remote browser"*, because the snapshot store holds sessions a human signed into on THIS machine |
| `brw_list_tab_groups` / `brw_group_tabs` / `brw_ungroup_tabs` | works | not advertised; calling one anyway errors: *"tab grouping is unavailable on any CDP transport"* | not advertised; same error | not advertised; same error | not advertised; same error |
| `brw_set_geolocation` / `brw_set_network_conditions` / `brw_emulate_media` / `brw_set_extra_headers` / `brw_set_user_agent` / `brw_set_locale` / `brw_init_script` / `brw_authenticate` | not advertised at all; calling one anyway errors: *"page environment overrides … are not supported on the extension-bridge transport"* | works | works | works | works |
| `brw_set_download_path` | not advertised; same error | works | not advertised; calling it anyway errors: *"brw will not choose where downloads land on a transport that drives the browser you are signed into"* | not advertised; brw did not start this browser, so it leaves the destination alone | not advertised; the directory would be created on the provider's disk |
| `brw_downloads` | works, with paths | works, with paths into brw's staging directory | works, `file_paths: false` and no path: the file went where the human's browser sends downloads | works, `file_paths: false` and no path | not advertised; the bytes land on the provider's disk, so there is no path here to report |
| `brw_upload_file` | works | works | works | works | not advertised; the path would name a file on the provider's disk, not the one you meant |
| `brw_clipboard` | not advertised; it needs the browser target the bridge cannot attach to | works | works | works | not advertised; the clipboard belongs to the machine the browser runs on |
| `brw_snapshot {include_ax:true}` | no AX tree | AX enrichment available | AX enrichment available | AX enrichment available | AX enrichment available |
| tab ids | Chrome tab ids, e.g. `"235935869"` | CDP target ids, e.g. `"79F95D14…"` | CDP target ids, e.g. `"79F95D14…"` | CDP target ids | CDP target ids |

All five transports ship in brw, and an unavailable capability is a property of
this profile's lane, not of the product; an operator can run a second daemon on
another transport. Most tools are listed and fully described in `tools/list` on
every lane and fail only when called. The seven page-environment tools are the
exception: they are DevTools session overrides that the bridge's attach/detach
cycle would silently drop between calls, so on the bridge they are not
advertised and an agent never spends a call finding out.

`brw_set_download_path` is the one of those seven that three other lanes also
lack, and not for want of the protocol. On `chrome-opt-in-cdp` and `remote-cdp`
the command applies to a whole browser context brw did not open, so pointing it
at brw's staging directory would move files somebody else downloads by hand;
downloads are still reported there, they just carry no path. On `off-host-cdp`
the directory it named would be created on the provider's machine. If a flow
needs the downloaded bytes — a digest assertion, or capturing the file as an
artifact — ask for a direct-CDP profile.

`chrome-opt-in-cdp` is the lane a person has to turn on for themselves, so you
will rarely see it: it needs Chrome 144+ and a human switching remote debugging
on at `chrome://inspect/#remote-debugging`. Never tell a user brw can enable it
— it cannot, by design. If they want incognito or HttpOnly cookies against their
own signed-in Chrome, that page is where they go; `brwctl doctor` prints the
same instruction.

`remote-cdp` and `off-host-cdp` both attach to a browser brw did not start, and
they differ in the one way that decides what a path means: `--remote` is pointed
at an endpoint on this machine, so an upload, the clipboard and a download
destination still name the things you meant, while a provider's browser shares
none of them. Read the transport rather than inferring from "remote".

On `off-host-cdp` the guards do not change. The navigation allow/block policy,
subresource containment, the site-consent gate and the identity guard all apply
exactly as they do locally — a browser on another machine is treated as less
trusted than a local one, not more. What changes is that there is no profile on
it, and no route to one: a recipe declaring `"requires": ["profile_session"]` is
refused before its first action rather than run signed out, and `brw_state` is
refused in all four of its actions so a session a human signed into on this
machine cannot be replayed into somebody else's browser, listed by a cloud-backed
run or deleted by one. The provider states a session lifetime past which every
call errors with *"the plugin-supplied browser session has expired"*; `GET
/health` on the browser host names the session and that expiry.

When incognito is unavailable and you need isolation: use a second brw profile (two
signed-in identities), or ask the operator for a direct-CDP profile (`brwd` without
`--bridge`), which also unlocks `brw_cookies` for scrubbing auth state between runs.

