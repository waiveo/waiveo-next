# Runbook — First Photon

Bring one image, end-to-end through the new stack, onto a single screen: a
feeder + relay run beside the untouched legacy stack, and the `player-v3`
channel pairs, pins, leases, fetches the image direct, and renders it.

All addresses below are `192.0.2.0/24` placeholders — substitute your box's real
LAN IP. Adoption is **per-screen and reversible**; the rest of the fleet stays on
legacy the whole time.

## 0. Preconditions

- The feeder/relay binaries built for the box's arch (`GOOS=linux GOARCH=amd64
  go build ./cmd/waiveo-feeder ./cmd/waiveo-relay`).
- One dev/adoptable screen. Never power-cycle the others.
- The crypto self-check has passed on the target firmware at least once (§4) —
  it confirms `roEVPDigest` is byte-correct before pairing depends on it.

## 1. Pre-flight (headroom gate)

The box has died of disk-full before. On the box:

```
free -m         # confirm RAM headroom
df -h /         # confirm disk headroom; if tight: docker image prune -f
systemctl is-active docker && docker compose -f /opt/waiveo/docker-compose.yml ps
```

Confirm the legacy stack is healthy on its own ports (web :5173, mgmt :80/:443)
before touching anything.

## 2. Deploy feeder + relay (non-conflicting ports)

```
# on the box
sudo useradd --system --home /opt/waiveo-next waiveo   # once
sudo install -d -o waiveo -g waiveo /opt/waiveo-next/bin
sudo install -o waiveo -g waiveo waiveo-feeder waiveo-relay /opt/waiveo-next/bin/

# edit the two unit files: set WAIVEO_FEEDER_CONTENT_URL and
# WAIVEO_RELAY_PAIR_HOST to the box's LAN IP (NOT 127.0.0.1 — a screen must
# reach these).
sudo cp deploy/systemd/waiveo-feeder.service deploy/systemd/waiveo-relay.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now waiveo-feeder waiveo-relay
```

Verify:

```
systemctl status waiveo-feeder waiveo-relay          # both active
curl -sk https://127.0.0.1:7420/healthz              # feeder ok
curl -sk https://127.0.0.1:7421/healthz              # relay ok
journalctl -u waiveo-relay | grep -m1 'pairing code' # note the emitted code
```

The relay log line `listening (HTTPS) on 0.0.0.0:7421 (pairing code dial
<LAN-IP>:7421)` must show the LAN IP, not loopback. Optionally run the Task-11
conformance drivers against the live box to confirm the surfaces conform.

## 3. Confirm the player crypto on the target firmware (once)

Sideload `player-v3` and launch the console-only self-check — no screen needed,
read the result from the debug console:

```
# package + sideload player-v3 (rokudev dev credentials), then:
curl -d '' "http://<dev-roku-ip>:8060/launch/dev?selfcheck=1"
telnet <dev-roku-ip> 8085     # watch for [RESULT] lines
```

Every `[RESULT] ... = PASS` (especially `roEVPDigest sha256(empty) vs known
vector` and `commitment(SPKI) == golden hex`) must pass before proceeding. A
FAIL here means the on-device crypto diverges from the Go oracle — stop and fix
the primitive, do not pair.

## 4. Adopt the screen (reversible)

1. **Suppress legacy control of THIS screen only** — so the legacy watchdog does
   not fight the new player for the screen (see the auto-launch/flapping
   history). This is a configuration change on the legacy stack, not a code
   change, so "legacy untouched" still holds. *(Confirm the exact
   unclaim/suppress step against the running legacy stack before executing.)*
2. Sideload `player-v3` to the screen.
3. Hand it the pairing code from step 2:
   ```
   curl -d '' "http://<screen-ip>:8060/launch/dev?pairingCode=<CODE-FROM-RELAY-LOG>"
   ```

## 5. First photon + verify the invariants

- The screen pairs, pins (local OOB commitment check), redeems, pulls its lease,
  fetches the image **direct from the feeder**, verifies `asset_ref`, and renders
  it full-screen. Capture a photo for the record.
- **Relay stayed out of the data path:** the feeder access path shows the content
  `GET`; the relay served no asset bytes.
- **Never-wipe:** relaunch the channel; the token + relay address persist (it
  renders without re-pairing).
- **Fleet untouched:** spot-check one other screen is still served by legacy
  (do NOT power-cycle it).

## 6. Rollback (per-screen dial)

1. Re-sideload the previous player channel to the screen.
2. Re-enable legacy control of the screen (reverse step 4.1).
3. Optionally stop the new stack: `sudo systemctl disable --now waiveo-relay
   waiveo-feeder`.

The other screens were never touched, so rollback is a single-screen operation.

## Known risk

The program poll sends its `content_types` capabilities as a JSON body on a GET
(`roUrlTransfer` `SetRequest("GET")` + `AsyncPostFromString`). If a firmware
drops the GET body, the relay's content-type gate returns zero content items and
the screen shows a status message instead of the image. If that happens, add a
query-string/header capabilities fallback to `GET /player/v1/program`.
