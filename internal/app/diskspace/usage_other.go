//go:build !unix

package diskspace

// usage reports ErrUnsupported on a platform with no statfs. The caller renders
// "unknown" rather than a zero that would read as a full disk.
func usage(string) (Usage, error) { return Usage{}, ErrUnsupported }
