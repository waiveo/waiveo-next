package virtualplayer

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	"github.com/maaxton/waiveo-next/internal/shared/paircode"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// TakeoverStartLatencyMillis is the fixed offset between a preemption's
// delivery instant (when the interrupted asset's render/end is stamped, PLY-101)
// and the takeover asset's own render/start (PLY-110) — the modeled swap
// latency this software player reports. player/1 leaves the real settle delay
// to measurement (Conformance notes); corpus cases exercise the ordering and
// the arithmetic this produces, not elapsed real time.
const TakeoverStartLatencyMillis int64 = 100

// ActiveRender is a player's currently-active Lease and the content asset it
// is mid-render of — the "before" state a newly delivered Lease preempts
// (PLY-094: a player holds exactly one active Lease at a time). Its fields
// mirror the PLY-101 corpus case's own active_lease_before + currently_rendering
// inputs, plus the active lease's own display (PLY-093) — without which a
// takeover cannot honor PLY-104's off-air rule.
type ActiveRender struct {
	LeaseID         string // the active lease's lease_id
	Priority        string // the active lease's priority: "scheduled" or "preempt" (PLY-100)
	Display         string // the active lease's display (PLY-093): "content" or "blank"
	ProgramRevision string // the active lease's program_revision (PLY-095), stamped on its render/end
	AssetRef        string // the asset currently on screen
	RenderStartTS   int64  // when that asset's render/start was reported (PLY-110)
}

// RenderEndReport is the subset of a player/1 render/end (PLY-111/112) the
// preempt swap produces and the PLY-101 oracle asserts: the asset that ended,
// the t_end at which it ended, and the cause/completion classifying the ending
// (both drawn from PLY-112's enumerations).
type RenderEndReport struct {
	AssetRef   string
	TEnd       int64
	Cause      string
	Completion string
}

// RenderStartReport is a player/1 render/start (PLY-110): the lease and asset a
// render began under, and when.
type RenderStartReport struct {
	LeaseID  string
	AssetRef string
	TS       int64
}

// Adoption is the outcome of a player adopting a newly delivered Lease over
// its currently-active one (PLY-094/100/101/107): the ack it returned
// (PLY-091), whether it interrupted the current render immediately, the
// render/end it reported for the interrupted asset, the render/start it
// reported for the takeover asset, the priority class the takeover render is
// classified under (PLY-107), the now-sole-active lease_id (PLY-094), and —
// when driven against a live relay by Preempt — the verified takeover content
// bytes.
type Adoption struct {
	AckLeaseID             string
	AckAccepted            bool
	InterruptedImmediately bool
	WaitedForNaturalEnd    bool
	PreviousRenderEnd      RenderEndReport
	TakeoverRenderStart    RenderStartReport
	TakeoverCause          string
	ActiveLeaseAfter       string
	TakeoverBytes          []byte
}

// AdoptionOf computes how a player adopts newly delivered Lease next over its
// currently-active render active, at wall-clock deliveryTime (ms) — the pure,
// network-free heart of player/1's priority-adoption rules
// (PLY-094/100/101/102/107):
//
//   - A preempt-priority next is adopted immediately, interrupting the current
//     render — the player MUST NOT wait for its natural end (PLY-101). Its
//     interrupted asset's render/end carries completion=interrupted; a
//     scheduled-priority next is adopted at the current asset's natural
//     boundary instead (PLY-102), completing it.
//   - The interrupted asset's render/end cause is the priority class it was
//     itself rendering under (PLY-107), stamped at deliveryTime.
//   - The takeover render/start (PLY-110) is the new lease's first content
//     item, TakeoverStartLatencyMillis after deliveryTime — but only when next
//     actually renders content: a display:blank next shows nothing (PLY-093),
//     and even a preempt one MUST NOT force a blanked screen back to visible
//     content (PLY-104), so no takeover render begins.
//   - next becomes the player's sole active Lease (PLY-094).
//
// PLY-104 off-air rule: when the active Lease's own display was blank, that
// blank is an intentional off-air state (PLY-155) and a preempt-priority next
// MUST NOT force the screen out of it — the screen keeps rendering blank until
// a SUBSEQUENT Lease's own display says otherwise. In that case nothing is
// rendering to interrupt and nothing starts: no interrupted render/end and no
// takeover render/start occur, though the new Lease is still accepted and
// becomes the sole active Lease (PLY-091/094). This is the virtualplayer
// sibling of the relay-side AdoptPreemptDisplay (playerserver, PLY-104); the
// two MUST agree, and the one that drives real render I/O (Preempt) is this one.
//
// It performs no I/O (TakeoverBytes stays nil); Preempt drives this against a
// live relay and fills in the verified bytes.
func AdoptionOf(active ActiveRender, next wire.Lease, deliveryTime int64) Adoption {
	preempt := next.Priority == "preempt"

	a := Adoption{
		AckLeaseID:       next.LeaseID,
		AckAccepted:      true,
		ActiveLeaseAfter: next.LeaseID,
		TakeoverCause:    causeForPriority(next.Priority),
	}

	// PLY-104: a preempt Lease arriving at a screen whose active Lease had
	// display:blank does not force it visible. There is no foreground render
	// to interrupt and none begins — the lease is accepted and active
	// (PLY-091/094) but the screen keeps rendering blank. Leaving
	// InterruptedImmediately/WaitedForNaturalEnd/PreviousRenderEnd/
	// TakeoverRenderStart at their zero values is exactly this "no render
	// transition" outcome.
	if preempt && active.Display == "blank" {
		return a
	}

	completion := "completed"
	if preempt {
		completion = "interrupted"
	}
	a.InterruptedImmediately = preempt
	a.WaitedForNaturalEnd = !preempt
	a.PreviousRenderEnd = RenderEndReport{
		AssetRef:   active.AssetRef,
		TEnd:       deliveryTime,
		Cause:      causeForPriority(active.Priority),
		Completion: completion,
	}

	if next.Display == "content" && len(next.Content) > 0 {
		a.TakeoverRenderStart = RenderStartReport{
			LeaseID:  next.LeaseID,
			AssetRef: next.Content[0].AssetRef,
			TS:       deliveryTime + TakeoverStartLatencyMillis,
		}
	}

	return a
}

// causeForPriority maps a Lease's priority (PLY-100) to the Render
// acknowledgement cause a render under it is reported with (PLY-107):
// preempt -> "preempted", scheduled -> "scheduled".
func causeForPriority(priority string) string {
	if priority == "preempt" {
		return "preempted"
	}
	return "scheduled"
}

// Preempt drives the full interrupt-now takeover against a LIVE relay serving
// a preempt-priority program: for pairingCode it establishes the pinned
// session exactly as Photon does (decode -> bootstrap pin -> redeem), pulls
// the program Lease, verifies its signature against the pinned relay cert
// (PLY-090), then — treating active as the scheduled render already on screen —
// adopts the pulled Lease per AdoptionOf. On a preempt Lease it acknowledges
// the new lease (PLY-091), reports the interrupted asset's render/end under the
// old lease and the takeover's render/start under the new one (PLY-110/111/114
// ordering: ack before any render/start), and fetches + content-address-verifies
// the takeover asset's bytes DIRECT from the feeder (PLY-084), returning them
// on the Adoption. On any failure it returns a zero Adoption and a wrapped
// error, never a partial result.
func Preempt(pairingCode string, active ActiveRender, deliveryTime int64) (Adoption, error) {
	host, port, grantSelector, commitment, err := paircode.Decode(pairingCode)
	if err != nil {
		return Adoption{}, fmt.Errorf("virtualplayer: Preempt: decode pairing code: %w", err)
	}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	certDER, err := bootstrapFetchAndPin(addr, commitment)
	if err != nil {
		return Adoption{}, fmt.Errorf("virtualplayer: Preempt: %w", err)
	}
	relayPub, err := publicKeyFromCertDER(certDER)
	if err != nil {
		return Adoption{}, fmt.Errorf("virtualplayer: Preempt: relay certificate public key: %w", err)
	}

	client := pinnedClient(commitment)
	base := "https://" + addr

	pairResp, err := redeem(client, base, grantSelector)
	if err != nil {
		return Adoption{}, fmt.Errorf("virtualplayer: Preempt: redeem: %w", err)
	}
	if pairResp.PairingStatus != "redeemed" || pairResp.ChannelToken == "" {
		return Adoption{}, fmt.Errorf("virtualplayer: Preempt: redeem: pairing_status = %q, want %q with a channel_token", pairResp.PairingStatus, "redeemed")
	}

	lease, err := pullProgram(client, base, pairResp.ChannelToken)
	if err != nil {
		return Adoption{}, fmt.Errorf("virtualplayer: Preempt: program: %w", err)
	}
	if err := verifyLeaseSignature(lease, relayPub); err != nil {
		return Adoption{}, fmt.Errorf("virtualplayer: Preempt: %w", err)
	}

	adopt := AdoptionOf(active, lease.Lease, deliveryTime)

	// PLY-091: acknowledge the new Lease before any render/start under it
	// (PLY-114).
	if err := ackLease(client, base, pairResp.ChannelToken, lease.LeaseID); err != nil {
		return Adoption{}, fmt.Errorf("virtualplayer: Preempt: lease ack: %w", err)
	}

	// PLY-101 interrupt-now: report the interrupted asset's render/end under
	// the OLD lease it was rendering (PLY-111, full events/1 EVT-050 shape).
	if adopt.InterruptedImmediately {
		if err := postRenderEnd(client, base, pairResp.ChannelToken, renderEndRequest{
			ScreenID:        lease.ScreenID,
			AssetRef:        adopt.PreviousRenderEnd.AssetRef,
			ProgramRevision: active.ProgramRevision,
			TStart:          active.RenderStartTS,
			TEnd:            adopt.PreviousRenderEnd.TEnd,
			Cause:           adopt.PreviousRenderEnd.Cause,
			Completion:      adopt.PreviousRenderEnd.Completion,
		}); err != nil {
			return Adoption{}, fmt.Errorf("virtualplayer: Preempt: interrupted-asset render/end: %w", err)
		}
	}

	// The takeover render/start + its verified content bytes, when the new
	// Lease renders content (AdoptionOf leaves TakeoverRenderStart zero for a
	// blank Lease, PLY-093/104).
	if adopt.TakeoverRenderStart.LeaseID != "" {
		if err := postRenderStart(client, base, pairResp.ChannelToken, renderStartRequest{
			LeaseID:  adopt.TakeoverRenderStart.LeaseID,
			AssetRef: adopt.TakeoverRenderStart.AssetRef,
			TS:       adopt.TakeoverRenderStart.TS,
		}); err != nil {
			return Adoption{}, fmt.Errorf("virtualplayer: Preempt: takeover render/start: %w", err)
		}

		item := lease.Content[0]
		imageBytes, err := fetchContent(item.URL)
		if err != nil {
			return Adoption{}, fmt.Errorf("virtualplayer: Preempt: fetch takeover content: %w", err)
		}
		if got := signhash.ContentID(imageBytes); got != item.AssetRef {
			return Adoption{}, fmt.Errorf("virtualplayer: Preempt: takeover content integrity check failed: fetched bytes hash to %q, lease asset_ref is %q", got, item.AssetRef)
		}
		adopt.TakeoverBytes = imageBytes
	}

	return adopt, nil
}

// renderStartRequest mirrors internal/relay/playerserver.RenderStartRequest
// (PLY-110): `{lease_id, asset_ref, ts}`.
type renderStartRequest struct {
	LeaseID  string `json:"lease_id"`
	AssetRef string `json:"asset_ref"`
	TS       int64  `json:"ts"`
}

// renderEndRequest mirrors internal/relay/playerserver.RenderEndRequest
// (PLY-111): events/1's content.played payload shape (EVT-050), field for
// field.
type renderEndRequest struct {
	ScreenID        string `json:"screen_id"`
	AssetRef        string `json:"asset_ref"`
	ProgramRevision string `json:"program_revision"`
	TStart          int64  `json:"t_start"`
	TEnd            int64  `json:"t_end"`
	Cause           string `json:"cause"`
	Completion      string `json:"completion"`
}

// postRenderStart performs PLY-110: POST /player/v1/render/start, presenting the
// channel token PLY-076 requires on every operation it authorizes — render
// acknowledgement and telemetry among them (PLY-070).
func postRenderStart(client *http.Client, base, channelToken string, req renderStartRequest) error {
	reqBody, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal RenderStart: %w", err)
	}
	resp, err := postAuthorized(client, base+"/player/v1/render/start", channelToken, reqBody)
	if err != nil {
		return fmt.Errorf("POST /player/v1/render/start: %w", err)
	}
	var out map[string]json.RawMessage
	return decodeOrProblem(resp, &out)
}

// postRenderEnd performs PLY-111: POST /player/v1/render/end, with the channel
// token (PLY-070/076). The body's own `screen_id` must be the one that token was
// issued to — a relay refuses a report filed against any other screen.
func postRenderEnd(client *http.Client, base, channelToken string, req renderEndRequest) error {
	reqBody, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal RenderEnd: %w", err)
	}
	resp, err := postAuthorized(client, base+"/player/v1/render/end", channelToken, reqBody)
	if err != nil {
		return fmt.Errorf("POST /player/v1/render/end: %w", err)
	}
	var out map[string]json.RawMessage
	return decodeOrProblem(resp, &out)
}
