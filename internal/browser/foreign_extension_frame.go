package browser

import "errors"

// ErrForeignExtensionFrame reports a tab Chrome will not let the extension
// bridge's debugger into because the page embeds another extension's frame,
// typically a password manager's autofill menu. The wrapped message names the
// extension and the frame URL when the extension saw the frame commit.
var ErrForeignExtensionFrame = errors.New("foreign_extension_frame")

// ForeignExtensionFramePrefix is the text the extension starts that refusal
// with, so the bridge can re-type it as ErrForeignExtensionFrame.
const ForeignExtensionFramePrefix = "foreign_extension_frame:"
