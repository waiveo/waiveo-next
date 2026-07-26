// Package channelinstall holds the integration test for
// scripts/install-from-channel-index.sh, the channel-index/1
// (contracts/channel-index.md) install/update client for a relay's own
// `relay-release` build.
//
// The script itself is intentionally plain POSIX sh (no bashisms) so it runs
// unmodified on a minimal appliance image with no Go toolchain -- there is
// nothing to import from it. This package exists only to exercise it
// end-to-end: a fixture channel-index/1 ChannelIndex document and artifact
// bytes served over a real httptest.Server, the script run as a real
// subprocess against an INSTALL_ROOT under t.TempDir(), and the resulting
// filesystem state (and the script's exit code) asserted against.
package channelinstall
