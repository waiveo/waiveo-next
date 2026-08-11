package derive

import "encoding/base64"

// base64Std encodes bytes for a data: URL. It is a one-line wrapper so page.go
// reads as page building rather than as encoding, and so the encoding used for
// the embedded font is named in exactly one place.
func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
