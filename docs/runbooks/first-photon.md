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
sudo chown waiveo:waiveo /opt/waiveo-next   # daemons create .dev/ under the workdir
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

### 2a. Upgrading a box that already has a store

Skip this on a genuinely fresh box — there is nothing to check. Do it on any box
that has run a feeder before, **including the lab appliance.**

Every row id has to be a canonical ULID, and the store validates the whole row
set on every write. A store seeded before that rule landed is therefore readable
but **write-dead**: it serves screens, and every attempt to author anything fails
with `ROW_ID_INVALID` naming a row nobody touched. The feeder repairs this at
boot — it rewrites the offending ids and every reference to them in one
transaction, touching nothing else, and leaves an already-conforming store
completely alone. Look before you restart anyway:

```
# on the box, as the waiveo user, with the feeder still running the old build.
# PASS THE PATH. The default store path is relative to the working directory and
# `sudo` does not change directory, so a command with no path reports on whatever
# `.dev/feeder-store.db` resolves to from wherever you happen to be standing.
sudo -u waiveo /opt/waiveo-next/bin/waiveo-feeder -store-check /opt/waiveo-next/.dev/feeder-store.db
```

The flag must come BEFORE the path: `waiveo-feeder <path> -store-check` leaves the
flag unparsed, and the binary refuses rather than starting the feeder against the
store you meant to inspect.

**Check the first line of the report.** It names the absolute path that was read
and the build that read it; if that is not the file you meant, nothing else in
the report is about your box.

It changes nothing in the store. It does leave `-shm`/`-wal` sidecars beside it,
which is normal for any reader of a WAL database — do not delete a sidecar you
did not create, and never delete a non-empty `-wal` (those frames are committed
data; `pragma integrity_check` answers "ok" on the emptied file afterwards).

Read the last line, `VERDICT:`, and the exit code — they say the same thing:

- **exit 0**, *"VERDICT: NOTHING TO DO"* — every section ran and answered, and
  the next boot changes nothing. Restart normally.
- **exit 2**, *"VERDICT: WORK PENDING"* — every section ran and answered, and the
  report lists what the boot will change (tables, columns, `old -> new` row-id
  rewrites, the first-seen seed, the events its retention sweep deletes) or what
  needs a person (schema drift, a declined org root, refused first-seen values, a
  store that moved while the report was being taken). The rewritten ids are
  derived from the old ones, so they are recognisable: restart and confirm the
  same list in `journalctl -u waiveo-feeder`.
- **exit 1**, *"VERDICT: INCOMPLETE"* — **stop.** Some section could not produce
  its answer (an unreadable ledger, an unopenable event log, a path holding no
  store at all), or the next boot will not serve. A missing section is a refusal,
  not a pass: do not restart until it is understood. Nothing has been changed —
  capture the output and the store file before doing anything else.

  **A command line this binary would not parse also exits 1** — a misspelled
  flag, an unknown flag, `-h`. None of them inspected a store, and none of them
  prints a `VERDICT:` line. If you are gating a deploy on the exit status, the
  absence of that line is the tell: `-store-chek` used to exit 2 and read as
  "work pending, deploy after reading".

Two of those readings used to be one: everything that was not one of three
specific throws exited 0, including a report about a file the check never opened.

**Running it against the LIVE store is supported and is what this step asks for
— and it now exits 2 rather than 0 on a busy box.** The report is taken section
by section with no snapshot, so a feeder writing underneath it can leave two
lines describing different states of the store. The check watches for that and
says so (`NOTE: … CHANGED while this report was being taken`). That is not a
fault in the store; it means only a person can decide whether the reading is good
enough to act on. If you want a clean 0, stop the feeder first:

```
sudo systemctl stop waiveo-feeder
sudo -u waiveo /opt/waiveo-next/bin/waiveo-feeder -store-check /opt/waiveo-next/.dev/feeder-store.db
```

Take a copy of the store first regardless; it is one file and it is the only
place authored content lives:

```
sudo -u waiveo cp -a /opt/waiveo-next/.dev/feeder-store.db{,.pre-upgrade}
```

After the restart, the store's generation advances by one, so every connected
relay re-pulls the desired state and picks the new ids up on its own — no relay
or player action is needed. Re-running the repair is safe: a second boot finds
nothing to do.

## 3. Confirm the player crypto on the target firmware (once)

Sideload `player-v3` and launch the console-only self-check — no screen needed,
read the result from the debug console:

```
# `make player-sideload` does the packaging + digest upload below for one
# device or the whole fleet, reading the dev password from the dev-lab env file
# (scripts/fleetsideload). Start with -dry-run:
#   make player-sideload SIDELOAD_ARGS="-dry-run -devices dev=<dev-roku-ip>"
#   make player-sideload SIDELOAD_ARGS="-devices dev=<dev-roku-ip>"
# The by-hand equivalent, for reference:
# zip MUST include testdata/ (the self-check reads pkg:/testdata golden vectors)
cd player-v3 && zip -r /tmp/player-v3.zip manifest source components testdata
# the form needs BOTH mysubmit and archive, or it 400s with "mysubmit Field Not Found"
curl --digest -u rokudev:<dev-pass> -F "mysubmit=Replace" \
  -F "archive=@/tmp/player-v3.zip" "http://<dev-roku-ip>/plugin_install"
# plugin_install AUTO-LAUNCHES the channel with no args. Exit to Home and WAIT
# until query/active-app reports type="home" before the deep-link launch, or
# the launch is a no-op against the running instance:
curl -d '' "http://<dev-roku-ip>:8060/keypress/Home"
curl -d '' "http://<dev-roku-ip>:8060/launch/dev?selfcheck=1"
# the 8085 console DROPS connections at app transitions — connect AFTER the
# app is up and read the session scrollback:
sleep 20 && nc <dev-roku-ip> 8085     # grep for [RESULT] lines
```

Every `[RESULT] ... = PASS` (especially `roEVPDigest sha256(empty) vs known
vector` and `commitment(SPKI) == golden hex`) must pass before proceeding. A
FAIL here means the on-device crypto diverges from the Go oracle — stop and fix
the primitive, do not pair.

## 4. Adopt the screen (reversible)

1. **Suppress legacy control of THIS screen only** — so the legacy watchdog does
   not fight the new player for the screen (see the auto-launch/flapping
   history). This is a configuration change on the legacy stack, not a code
   change, so "legacy untouched" still holds. Concretely (verified 2026-07-26):
   the per-screen kill switch is `slidecast_screens.watchdog_enabled`, settable
   over the authenticated web API —
   `PUT /api/extensions/slidecast/screens/<serial> {"watchdog_enabled": false}`
   (serial like `roku:X0290...`; reverse with `true` on rollback).

   The other half of that fight is closed on the new side too: the relay's own
   screen keep-alive (`internal/relay/keepalive`) runs by default but acts ONLY
   on entities the app peer's signed `device_inventory` marks adopted and
   enabled. A Roku this relay can reach but has not adopted is never
   re-launched by it, so step 4.1 and this gate cover the two directions of the
   same flapping failure. Set `WAIVEO_RELAY_KEEPALIVE=0` to disable it outright.
2. Sideload `player-v3` to the screen (`make player-sideload`, above).
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
*(Did NOT bite on firmware 15.2.4 — the GET body arrived intact.)*

## Hardware findings — first photon achieved 2026-07-26, The Hanger, fw 15.2.4

The full chain (pair → pin → redeem → program → lease → direct fetch →
`asset_ref` verify → render) worked after four on-device fixes, all caught by
the §3/§5 gates exactly as designed:

- `roEVPDigest` returns `""` for ZERO-LENGTH input (both `Process(empty)` and
  `Setup`+`Final()`); non-empty digests are byte-correct. `wvSha256Hex` now
  fails closed on empty input by contract.
- A committed fixture's trailing LF made three byte-identical self-check
  comparisons fail invisibly — `wvTrim` originally stripped CR but not LF.
- The Roku TLS stack refuses a CA:FALSE self-signed cert as a trust anchor
  and a clientAuth-only leaf as a server ("unsuitable certificate purpose").
  Fixed in `tlsboot.GenSelfSigned` (CA-shaped anchor) and feeder issuance
  (dual EKU). The virtualplayer's skip-verify+pin posture can never surface
  EKU/anchor-shape failures — only an ordinary-verification client does.
- Ed25519 TLS (leaf and CA) handshakes fine on this firmware.
- Re-pairing after a relay re-key needs no wipe: launch with the fresh code —
  a supplied code is deliberate re-provisioning and takes precedence over
  persisted state (persisted pairing survives if the new code fails).
