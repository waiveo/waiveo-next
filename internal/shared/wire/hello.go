package wire

// Hello is the relay -> app-peer message sent as frame zero after Challenge,
// on every fresh authenticated connection (relay/1 REL-030, REL-031). Field
// names are the contract's Wire-shapes spelling verbatim; this placeholder
// carries only the fields REL-031 requires — later tasks add the envelope
// (type/id) and handshake behavior around it.
type Hello struct {
	RelayID                 string         `json:"relay_id"`
	ProtocolVersion         string         `json:"protocol_version"`
	Features                []string       `json:"features"`
	SiteBinding             SiteBinding    `json:"site_binding"`
	SubnetMetadata          SubnetMetadata `json:"subnet_metadata"`
	ClockState              ClockState     `json:"clock_state"`
	ChannelBindingSignature string         `json:"channel_binding_signature"`
}

// SiteBinding is the site this relay is bound to, and that site's effective
// timezone and coordinates (relay/1 REL-036).
type SiteBinding struct {
	ScopeNode string  `json:"scope_node"`
	TZ        string  `json:"tz"`
	Lat       float64 `json:"lat"`
	Long      float64 `json:"long"`
}

// SubnetMetadata carries the relay's own canonical advertised address
// (relay/1 REL-037).
type SubnetMetadata struct {
	AdvertisedAddress string `json:"advertised_address"`
}

// ClockState carries the relay's clock trust state and time source
// (relay/1 REL-038).
type ClockState struct {
	State  string `json:"state"`
	Source string `json:"source"`
}
