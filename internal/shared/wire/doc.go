// Package wire holds Go types for the wire-shape messages the relay/1 contract
// (contracts/relay-1.md) defines for the relay <-> app-peer protocol. It is the
// shared vocabulary imported by both the feeder (cmd/waiveo-feeder) and the
// relay (cmd/waiveo-relay) skeletons, and by whatever app-peer component later
// waves add.
//
// Types here are contract-derived data shapes only — no protocol behavior
// (handshake sequencing, signing, negotiation) lives in this package. json
// struct tags MUST match the contract's Wire-shapes field names byte-for-byte;
// when the contract's field set changes, update the type here in the same
// change that bumps the contract.
package wire
