# Release signing

This is the buy-and-configure runbook for signing brw's installers. Everything in
the release pipeline is already written; what is missing is a paid Apple
membership and a Windows code-signing certificate. Once the secrets below exist,
the next tag produces signed installers with no code change.

Read this if you hold the credit card. You do not need to know how the release
workflow works.

## Where things stand

`.github/workflows/release.yml` looks for signing secrets. When they are absent it
falls back to today's behaviour and prints a warning annotation on the job:

| Platform | Without secrets | With secrets |
| --- | --- | --- |
| macOS | Binaries ad-hoc signed (`codesign --sign -`), `.pkg` unsigned. Gatekeeper says "unidentified developer". | Binaries signed with Developer ID Application, hardened runtime, secure timestamp; `.pkg` signed with Developer ID Installer, notarized by Apple, ticket stapled. |
| Windows | **Not built.** The MSI job is disabled because, with no Authenticode certificate, the MSIs shipped unsigned and only earned SmartScreen warnings plus a signing report nothing could act on. Restore the job per the comment in `.github/workflows/release.yml`. | Authenticode SHA-256 with an RFC 3161 timestamp over the four executables and each `.msi`. |
| Linux | `.deb` / `.rpm` unsigned. No change planned; there is no distribution GPG key. | Unchanged. |
| Relocatable `.tar.gz` | macOS binaries ad-hoc signed, Linux binaries unsigned. This is the archive `scripts/install.sh` and the Homebrew formula download. | Unchanged. `scripts/package-tarball.sh` ad-hoc signs on macOS whatever else is configured, because `install.sh` re-signs the unpacked copy the same way and would discard a Developer ID signature anyway. |

Every release, signed or not, publishes `SHA256SUMS.txt`, a CycloneDX SBOM, and a
GitHub build provenance attestation over every attached file. The release notes
carry the verify command. That is what a reader has today instead of a signature.

The release notes say which path the build took, and the job summary carries the
raw output of `codesign --verify`, `pkgutil --check-signature`, `spctl`,
`stapler validate` and `signtool verify`. If signing is configured and any of
those fail, the release fails rather than publishing something that claims to be
signed and is not.

## Part 1 — macOS

### 1.1 What to buy

Apple Developer Program membership, **$99 per year**, from
<https://developer.apple.com/programs/enroll/>.

- An **Individual** enrolment is enough to issue Developer ID certificates. The
  publisher name users see is then your legal personal name.
- An **Organization** enrolment shows the company name instead, but requires a
  D-U-N-S number for the legal entity and Apple verifying it. Allow one to four
  weeks; the D-U-N-S lookup alone can take days.

There is nothing else to buy. Notarization is included.

### 1.2 Create the two certificates

Both are made the same way, and you need both: one signs the executables, the
other signs the installer package.

1. On a Mac, open **Keychain Access** → menu **Certificate Assistant** →
   **Request a Certificate From a Certificate Authority**.
   - Enter your Apple ID email and your name.
   - Select **Saved to disk**, and **Let me specify key pair information**.
   - Key size 2048 bits, algorithm RSA. Save the `.certSigningRequest` file.
2. Go to <https://developer.apple.com/account/resources/certificates/list> →
   **+** → choose **Developer ID Application** → upload the CSR → download the
   `.cer` → double-click it to install into your login keychain.
3. Repeat step 1 and step 2 choosing **Developer ID Installer**. Use a fresh CSR.

You now have two certificates in Keychain Access under **My Certificates**, each
with a private key attached. Their names look like
`Developer ID Application: Your Name (ABCDE12345)` — the ten characters in
brackets are your Team ID.

Back up both. Apple lets you create a limited number of Developer ID
certificates per account and cannot re-issue the private key.

### 1.3 Export them as one .p12

1. In Keychain Access, **My Certificates**, select both `Developer ID …` rows
   (⌘-click the second). Each must show a disclosure triangle with a private key
   under it; if not, you selected the certificate without its key.
2. Right-click → **Export 2 items…** → format **Personal Information Exchange
   (.p12)** → save as `brw-developer-id.p12`.
3. Set a strong password when prompted. This is `MACOS_CERTIFICATE_PASSWORD`.
   Store it in your password manager; it is not recoverable.

### 1.4 Mint an App Store Connect API key for notarization

Notarization uploads the package to Apple for an automated malware scan. The
workflow authenticates with an API key rather than your Apple ID, so no
app-specific password and no two-factor prompt is involved.

1. Go to <https://appstoreconnect.apple.com/access/integrations/api> →
   **Team Keys** tab.
2. **+** → name it `brw release notarization` → access role **Developer**.
3. Download the `AuthKey_XXXXXXXXXX.p8`. **Apple serves this file once.** Save it
   to your password manager immediately.
4. Note two values from that page:
   - **Key ID** — the ten characters in the key's row, also in the filename.
   - **Issuer ID** — a UUID shown above the key list, shared by all your keys.

### 1.5 Find your Team ID

<https://developer.apple.com/account> → **Membership details** → **Team ID**. Ten
characters, the same string that appears in brackets in the certificate names.

### 1.6 Set the GitHub secrets

Run these on the Mac holding the `.p12` and the `.p8`. `gh secret set` without
`--body` reads the value from stdin or prompts, which keeps it out of your shell
history.

```sh
base64 < brw-developer-id.p12 | tr -d '\n' | gh secret set MACOS_CERTIFICATE_P12 --repo Don-Works/brw
gh secret set MACOS_CERTIFICATE_PASSWORD --repo Don-Works/brw   # paste the .p12 password

base64 < AuthKey_XXXXXXXXXX.p8 | tr -d '\n' | gh secret set APPLE_API_KEY_P8 --repo Don-Works/brw
gh secret set APPLE_API_KEY_ID --repo Don-Works/brw             # paste the 10-character Key ID
gh secret set APPLE_API_ISSUER_ID --repo Don-Works/brw          # paste the issuer UUID
gh secret set APPLE_TEAM_ID --repo Don-Works/brw                # paste the 10-character Team ID
```

Both base64 blobs must be a single line, which is what `tr -d '\n'` is for.

That is all six. The workflow reads the certificate names out of the imported
keychain, so you do not have to type them. If you ever hold more than one
Developer ID pair and need to pin which is used, set these two as well:

```sh
gh secret set MACOS_SIGN_IDENTITY --repo Don-Works/brw        # "Developer ID Application: Your Name (ABCDE12345)"
gh secret set MACOS_INSTALLER_IDENTITY --repo Don-Works/brw   # "Developer ID Installer: Your Name (ABCDE12345)"
```

### 1.7 Confirm it worked

Tag a release. In the `macos-package` job:

- The "Import Developer ID signing material" step should print no warning
  annotation. A warning titled "macOS artifacts are ad-hoc signed" means
  `MACOS_CERTIFICATE_P12` did not arrive; one titled "macOS artifacts are not
  notarized" means the API key trio did not.
- The "Verify macOS signing" step runs `codesign --verify --deep --strict` on
  each binary extracted back out of the built `.pkg`, `pkgutil --check-signature`
  on the package, asserts the Team ID matches `APPLE_TEAM_ID`, then
  `spctl -a -vvv -t install` and `xcrun stapler validate`. Its output lands in
  the job summary.

Then check a real download the way a user would:

```sh
spctl -a -vvv -t install brw_<version>_macos_universal.pkg
# source=Notarized Developer ID
xcrun stapler validate brw_<version>_macos_universal.pkg
# The validate action worked!
```

## Part 2 — Windows

### 2.1 OV versus EV, and what each does for SmartScreen

Two grades of publicly trusted code-signing certificate exist. Both assert a
verified organisation or individual; they differ in vetting depth, price, and how
SmartScreen treats them.

**OV (Organization Validation)** — the CA verifies the legal entity through
business registries and a callback. Cheaper. A newly signed installer still trips
SmartScreen's "Windows protected your PC" dialog. Reputation then accrues to the
publisher identity as downloads accumulate without incident, and once it is
established it carries to later releases signed by the same certificate.
Timescale is not published by Microsoft and depends on download volume; for a
project with brw's numbers, assume months rather than days.

**EV (Extended Validation)** — deeper vetting, and the private key must live on a
hardware token or qualified HSM. Historically EV granted immediate SmartScreen
reputation, which was its main selling point. Microsoft has since moved
SmartScreen toward identity-based reputation shared across certificates, so EV no
longer guarantees a clean first download, though it still reaches reputation
faster than OV. EV is mandatory only for kernel-mode driver signing, which brw
does not do.

**The recommendation for brw: OV.** The premium for EV buys a SmartScreen head
start that Microsoft no longer guarantees, on a product that ships a CLI and a
daemon rather than a consumer double-click app.

### 2.2 What you can actually buy

Since 1 June 2023 the CA/Browser Forum requires the private key of **every**
publicly trusted code-signing certificate, OV and EV alike, to be generated and
held in hardware certified to FIPS 140-2 Level 2 or Common Criteria EAL4+. No CA
will sell you an exportable `.pfx` any more. You get one of:

- a **USB token** posted to you (SafeNet eToken). Unusable from a
  GitHub-hosted runner; it would tie releases to one physical machine.
- a **cloud signing service** that holds the key and signs on request. This is
  the option that works in CI.

Realistic choices, cheapest first. Confirm current pricing before buying, these
move:

| Service | Roughly | Notes |
| --- | --- | --- |
| Azure Trusted Signing | ~$10/month (Basic tier) | Publicly trusted, OV-equivalent. Needs an Azure subscription. Organisation identity validation requires three or more years of verifiable public existence; there is an individual option. Signs via a signtool plugin. |
| SSL.com eSigner | ~$250–400/year | OV with cloud signing included. CodeSignTool or an eSigner signtool plugin. |
| DigiCert KeyLocker | ~$600+/year | OV plus the KeyLocker HSM service. PKCS#11 library and `smctl`. |
| Certum Open Source Code Signing | ~€100–150/year | Cheapest certificate, but the standard product ships a USB token. Only useful here if you take their cloud option. |

Azure Trusted Signing is the cheapest route that a GitHub Actions job can use.

### 2.3 Route A — you have an exportable .pfx

This applies to a certificate issued before June 2023, or one from an internal
CA. The workflow supports it directly:

```sh
base64 < brw-codesign.pfx | tr -d '\n' | gh secret set WINDOWS_CERTIFICATE_PFX --repo Don-Works/brw
gh secret set WINDOWS_CERTIFICATE_PASSWORD --repo Don-Works/brw   # paste the .pfx password
```

Optionally pin the timestamp authority to match your CA (the default is
`http://timestamp.digicert.com`):

```sh
gh variable set WINDOWS_TIMESTAMP_URL --repo Don-Works/brw --body "http://timestamp.sectigo.com"
```

The workflow writes the `.pfx` to the runner's temp directory, imports it into
the per-run certificate store, signs by thumbprint so the password never reaches
a command line, and deletes both the file and the imported certificate whether
the job succeeds or fails.

### 2.4 Route B — a cloud signing service

`scripts/package-windows.ps1` takes the signtool plugin interface these services
use. Point it at the vendor's dispatch library and metadata file:

```sh
gh variable set WINDOWS_SIGN_DLIB --repo Don-Works/brw --body 'C:\signing\Azure.CodeSigning.Dlib.dll'
gh variable set WINDOWS_SIGN_DMDF --repo Don-Works/brw --body 'C:\signing\metadata.json'
gh variable set WINDOWS_TIMESTAMP_URL --repo Don-Works/brw --body "http://timestamp.acs.microsoft.com"
```

These are repository variables, not secrets: they are paths, and the credential
never appears in them.

**The `windows-packages` job is currently disabled** (see the Windows row in the
table above): the release workflow does not build MSIs at all, so nothing below
runs today. This section is the restore path — re-enable the job first, per the
comment in `.github/workflows/release.yml`, then work through it.

**One step is still missing and has to be added when you pick a vendor.** The
plugin authenticates to the service on its own, and nothing in
`windows-packages` does that yet. For Azure Trusted Signing that means an
`azure/login` step using OIDC federated credentials plus a step that downloads
the dispatch library to the path above, both placed before "Build Windows
package". Vendor instructions differ enough that guessing them here would be
worse than leaving the gap named. Setting `WINDOWS_SIGN_DLIB` without that step
fails the build loudly rather than publishing unsigned MSIs.

### 2.5 Confirm it worked

In the `windows-packages` job, "Verify Windows signing" runs
`signtool verify /pa /v` on each `.msi` and fails the release if the signature,
chain or timestamp does not check out. The output goes to the job summary.

On a downloaded file:

```powershell
signtool verify /pa /v brw_<version>_windows_amd64.msi
# or, without the Windows SDK:
Get-AuthenticodeSignature .\brw_<version>_windows_amd64.msi | Format-List
```

`Status` must be `Valid` and `SignerCertificate` must name your organisation.

## Part 3 — the Homebrew tap token

Not a signing credential, but the other secret the release workflow reads, and
this one is free.

`brew install don-works/tap/brw` resolves to `github.com/Don-Works/homebrew-tap`,
a repository separate from this one. After `publish-release` succeeds, the
`homebrew-tap` job renders the formula from `packaging/homebrew/brw.rb` with the
version and the four tarball sha256 sums filled in, and commits it to that
repository as `Formula/brw.rb`. The sums come from the artifacts the release just
built, so nothing is fetched over the network to find them.

`github.token` is scoped to `Don-Works/brw` and cannot write to another
repository, so the commit needs a token of its own.

### 3.1 Mint the token

A fine-grained personal access token, from
<https://github.com/settings/personal-access-tokens/new>:

- **Resource owner**: `Don-Works`.
- **Repository access**: only select repositories, and pick only
  `Don-Works/homebrew-tap`.
- **Permissions** → Repository permissions → **Contents: Read and write**.
  Nothing else.
- **Expiration**: pick a date and put the renewal in your calendar.

```sh
gh secret set HOMEBREW_TAP_TOKEN --repo Don-Works/brw   # paste the token
```

### 3.2 What happens with it and without it

- **Absent**: the `homebrew-tap` job prints a warning annotation naming
  `HOMEBREW_TAP_TOKEN`, writes the rendered formula into the job summary, and
  succeeds. The release publishes as normal, and `brew install don-works/tap/brw`
  keeps installing the previous version until someone commits that formula to
  `Don-Works/homebrew-tap` as `Formula/brw.rb`. A missing tap token never fails a
  release.
- **Present and able to write**: the job commits `Formula/brw.rb` with the
  message `brw <version>` and prints the resulting commit URL.
- **Present but expired, revoked, or scoped to the wrong repository**: the job
  fails, the same way a broken signing secret does. The release itself is already
  published by then, so there is nothing to roll back — fix the token and use
  "Re-run failed jobs" on that workflow run.

The formula is also in the job summary on every release, token or not, so a bump
can always be done by hand from the run page.

## Secret and variable reference

Repository secrets, `gh secret set <NAME> --repo Don-Works/brw`:

| Name | Contains | Required for |
| --- | --- | --- |
| `MACOS_CERTIFICATE_P12` | Single-line base64 of the `.p12` holding both Developer ID certificates and their private keys | macOS signing |
| `MACOS_CERTIFICATE_PASSWORD` | The password set when exporting that `.p12` | macOS signing |
| `MACOS_SIGN_IDENTITY` | Optional. Full Developer ID Application identity name, to pin which certificate is used | macOS signing |
| `MACOS_INSTALLER_IDENTITY` | Optional. Full Developer ID Installer identity name | macOS signing |
| `APPLE_TEAM_ID` | Ten-character Team ID. The build asserts the shipped binaries carry it | macOS verification |
| `APPLE_API_KEY_P8` | Single-line base64 of the App Store Connect `AuthKey_*.p8` | Notarization |
| `APPLE_API_KEY_ID` | Ten-character App Store Connect Key ID | Notarization |
| `APPLE_API_ISSUER_ID` | App Store Connect issuer UUID | Notarization |
| `WINDOWS_CERTIFICATE_PFX` | Single-line base64 of an exportable `.pfx` | Windows signing, route A |
| `WINDOWS_CERTIFICATE_PASSWORD` | The `.pfx` password | Windows signing, route A |
| `HOMEBREW_TAP_TOKEN` | Fine-grained PAT with Contents: Read and write on `Don-Works/homebrew-tap` and nothing else | Committing `Formula/brw.rb` to the tap after a release |

Repository variables, `gh variable set <NAME> --repo Don-Works/brw`:

| Name | Contains |
| --- | --- |
| `WINDOWS_TIMESTAMP_URL` | RFC 3161 timestamp authority. Defaults to `http://timestamp.digicert.com` |
| `WINDOWS_SIGN_DLIB` | Path on the runner to a cloud signing service's signtool dispatch library |
| `WINDOWS_SIGN_DMDF` | Path on the runner to that service's signing metadata JSON |

Each group is independent. Setting only the macOS secrets signs the `.pkg`; the
Windows variables do nothing until the `windows-packages` job is re-enabled.
`HOMEBREW_TAP_TOKEN` is independent of both and buys nothing about signatures.

## Renewals and what expires

- **Apple Developer Program**: annual. Lapse it and the certificates stop being
  valid for new signatures. Already-notarized releases keep working.
- **Developer ID certificates**: five years. The `.p12` must be re-exported and
  `MACOS_CERTIFICATE_P12` re-set before then.
- **App Store Connect API key**: does not expire, but can be revoked. Revoking it
  breaks notarization only, not signing.
- **Code-signing certificate**: one to three years depending on what you buy.
- **`HOMEBREW_TAP_TOKEN`**: whatever expiry you chose when minting it. Expiry
  fails the `homebrew-tap` job rather than skipping it, because a token that is
  present and rejected is indistinguishable from a token that is wrong.
- **Timestamps**: the reason `/tr` and `--timestamp` are used everywhere. A
  timestamped signature stays valid for what it signed after the certificate
  expires. Without one, every shipped installer breaks on the expiry date.

## Two things worth doing once the secrets exist

- Put the signing secrets in a GitHub **Environment** with required reviewers,
  and set `environment:` on the `macos-package` and `windows-packages` jobs. As
  repository secrets they are readable by any workflow run that a push to the
  default branch can trigger, which is a wider blast radius than a release needs.
- Sign a throwaway tag such as `v0.0.0-signing-test` first and check both job
  summaries before signing a real release. The signing path is exercised for the
  first time by whichever tag you push after setting the secrets.

## If a release fails after you set the secrets

The fallback is deliberately one-way: absent secrets mean an unsigned build,
present-but-broken secrets mean a failed build. A release that failed here never
published anything, so there is nothing to roll back — fix the secret and push a
new tag.

Common causes:

- `security import` fails: `MACOS_CERTIFICATE_PASSWORD` does not match the
  `.p12`, or the base64 was wrapped across lines.
- "the keychain has no Developer ID Application and Developer ID Installer
  pair": the `.p12` was exported from the certificates without their private
  keys, or holds only one of the two.
- `notarytool` returns `Invalid`: read the log it points at. For brw the usual
  cause is a binary that reached the package without the hardened runtime, which
  means something re-wrote the payload after `codesign` ran.
- `signtool verify` fails on the timestamp: the timestamp authority was
  unreachable during the run. Re-run the job.
- `gh api repos/Don-Works/homebrew-tap` returns 404 or 403: `HOMEBREW_TAP_TOKEN`
  expired, or was minted without Contents: Read and write on that repository.
  This is the one failure on this page that happens after the release is
  published, because the tap bump runs last. The release is fine; only the
  `homebrew-tap` job needs re-running.
