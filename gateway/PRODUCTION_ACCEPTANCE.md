# camOS Gateway 1.0 physical production acceptance

This is a required evidence checklist, not an automated claim of production readiness. Complete it on real hardware and real services for each of the four targets before commercial distribution.

## Admission record

Record and retain:

- source commit SHA and clean-tree evidence;
- exact Go 1.25.12 native toolchain output for every target;
- successful native test, `go vet ./...`, build, and static-inspection output;
- the aggregate `SHA256SUMS` with all four executable hashes;
- successful `TestLocalReleaseSet` output;
- the immutable Artifact Registry repository/package/version;
- successful `TestRealArtifactRegistryReleaseContract` output; and
- operator, timestamp, device model, OS release, filesystem, and test-network details for every physical run.

The real-provider verifier is an admission gate only. It must never update `desired_version`.

## Control-plane prerequisites

Before connecting commercial candidates:

- migrate `desired_version` and `reported_version` to support the canonical string `1.0`;
- verify a control refresh writes `reported_version = '1.0'` before update confirmation can clear local recovery state;
- reserve `desired_state = 0 AND site_id IS NULL` exclusively for intentional terminal removal;
- make site moves, unassignment, and administrative workflows preserve that invariant transactionally; and
- require a separate explicit deployment decision after release admission.

No Gateway can infer human intent from two identical authoritative database states. Double revalidation reduces races but cannot compensate for a workflow that temporarily commits the terminal pair.

## First 1.0 installation boundary

Install 1.0 through the native/manual service path on each target. For an existing pilot, stop the old service, retain its canonical identity directory, replace/install through the approved native procedure, and restart under OS supervision. Do not use a pre-hardening self-update as evidence for the hardened lifecycle.

For each target prove:

- the same GatewayID remains in use where identity was migrated;
- runtime X.509/WIF authentication succeeds;
- an authoritative Postgres refresh succeeds and reports 1.0;
- heartbeat reaches the control plane;
- the assigned camera starts;
- FFmpeg is the embedded matching native payload; and
- expected GCS media transport succeeds.

Identity locations are `C:\ProgramData\camOS Gateway`, `/var/lib/camos-gateway`, and `/Library/Application Support/camOS Gateway`. Treat identity, private-key, and certificate evidence as sensitive; record presence/continuity, never their contents.

## Update and downgrade on every target

Build and admit a future test release through the same four-target process. On Windows amd64, Linux amd64, macOS Intel, and macOS Apple Silicon, test both directions:

```text
confirmed 1.0
→ explicit desired future version
→ verified download and replacement
→ same GatewayID
→ successful runtime authentication/control refresh
→ reported future version
→ camera and GCS resume

confirmed future version
→ explicit desired 1.0
→ the identical verified mechanism
→ same GatewayID
→ successful runtime authentication/control refresh
→ reported 1.0
→ camera and GCS resume
```

Capture the Artifact Registry resource name, expected/local/downloaded SHA-256, update lifecycle diagnostic, service transitions, and proof that `active.previous` plus `update.pending` disappear only after confirmation.

## Update fault matrix

Repeat representative cases on all targets and platform-specific cases below. Verify the safe outcome, not just an error message.

- Throttle a full executable transfer beyond the former 90-second window while bytes continue; it must finish.
- Stall with no meaningful byte progress; it must abort within the bounded stall policy and leave active bytes untouched.
- Disconnect mid-body, truncate the response, lie with `Content-Length`, omit `Content-Length`, and exceed the byte maximum; each must fail before replacement.
- Reboot/service-stop during `.downloading`; startup must discard the partial deterministically.
- Present wrong SHA, duplicate SHA, wrong size, wrong executable family, wrong target, wrong Go module, wrong BuildVersion, and a bad/missing stamp; none may threaten active bytes.
- Change desired version, desired state, site, organisation/site hierarchy, or restart request during download; the final fresh fingerprint must abort the attempt.
- Make the final DB reread fail; no replacement may occur.
- Introduce terminal authority during update; terminal removal must win.
- Fail/crash before previous-copy publication, after it, after `update.pending`, immediately before replacement, and immediately after replacement; classify pre/post commit correctly and converge without a release history.
- Fail the identity-directory sync after `update.pending` becomes visible; no active replacement may occur until an exact reread successfully proves the record durable.
- Make Postgres temporarily unavailable after the new process launches; retain the pending transition rather than declaring a healthy candidate bad.
- Fail an admitted candidate before its controller starts: the failure must trigger immediate last-known-good rollback. Repeated power loss before confirmation must consume only the two durable candidate-start admissions and force rollback on the next start, while a live candidate waiting on Postgres must not consume additional starts.
- Restore Postgres and prove authenticated refresh/reporting completes confirmation and cleans the one-slot fallback.
- Constrain disk space or inject filesystem/sync failures where practical; active authority must follow the documented commit boundary.
- Hold executable/FFmpeg file and process locks; prove bounded shutdown and native recovery behavior.
- Start duplicate Gateway/service processes and a concurrent manual service install while a throttled download is active. Prove the download lock permits fresh terminal authority to proceed, the lifecycle lock serializes candidate publication/recovery/installation, abandoned native locks recover after a killed owner, and every participant converges without a second pending transition.

At every interruption, assert there is at most one each of `candidate`, `candidate.downloading`, `active.previous`, and `update.pending`, with no accumulating release cache.

## Terminal-removal authority tests

Exercise the exact predicate and both fresh reads against real Postgres:

1. First read is terminal, then change either field before the second read: no marker and no deletion.
2. First read is terminal, then fail the second read: no marker and no deletion.
3. First read is terminal, then camera/FFmpeg cannot stop within the bound: no marker and no deletion.
4. Both reads are terminal but marker publication fails: no committed removal.
5. Both reads match and marker commits, then reverse the DB state: removal still completes and normal operation never resumes.
6. Commit removal, fail helper preparation, and leave the process alive: it must remain removal-only with no camera, update, renewal, or commissioning.
7. Reboot after marker commit at several cleanup points: the appliance must not resurrect and cleanup must converge.
8. Repeat cleanup with files, credentials, service definitions, or registry state already absent: absence must count as success.
9. Place an update candidate and pending update beside a valid removal marker: removal must dominate and delete update state.
10. Test valid GatewayID-bound, mismatching GatewayID, valid legacy, malformed, and unreadable markers. Ambiguous state must fail closed; a mismatch must never delete the newer identity blindly.
11. Attempt commissioning with every committed/ambiguous marker form: commissioning must refuse.
12. Fail the identity-directory sync after a removal marker becomes visible: no deletion may start until an exact reread successfully proves it durable. If a native removal unit survives without a valid marker, the permanent POSIX service must remain fail-closed.
13. Pause an old commissioning CLI and a certificate renewal before their final writes, complete removal and establish a replacement identity, then resume them. The old processes must reject the changed directory inode/GatewayID/hash/key and must not publish into or clean the replacement generation.
14. On Linux and macOS, interrupt immediately before and after the helper atomically renames the entire canonical identity directory to its fixed `.removing` sibling. Before the rename, only the validating helper may progress. After the rename, stale flock waiters must fail canonical-inode validation and the native unit/job must finish the tombstone after reboot without recreating or recursively deleting a canonical identity path.
15. Fail native removal-unit/plist cleanup after the `.removing` directory is gone. On retry, native cleanup must finish while the canonical identity path remains wholly absent; any unexpected canonical path or malformed/symlink tombstone must fail closed for operator remediation.

After legitimate completion and a reboot, prove absence of Gateway and FFmpeg processes, native service definition, installed executable and embedded FFmpeg, Gateway identity, private key/certificate, meaningful work/update state, and Windows Gateway registry identity.

## Windows amd64 probes

Use service `camOSGateway` and real NTFS/SCM behavior.

- Validate replacement while the active executable is locked and the transient helper waits for the service process to exit.
- Validate every required `MoveFileEx`/delayed-delete registration and a reboot that completes registered deletion.
- Prove the service is first redirected to a cryptographically unique transient removal helper, package deletion is registered, and the stopped automatic service remains as boot-time finalizer authority. A successful removal handoff must be reported to SCM as an orderly stop so the ordinary 30-second failure action cannot launch a concurrent removal actor.
- On the first cleanup reboot, prove the package queue is consumed before SCM starts the helper. The helper must then register the remaining marker, unique helper, work directory, and identity directory for the next reboot before deleting the service.
- Until that final cleanup reboot, verify `removal.pending` and the removal-helper tombstone remain visible, commissioning/reinstallation is refused, and no normal service/camera work can resume. Afterward prove the delayed-delete queue is fully consumed before any canonical path can be reused.
- Inject partial registration, service-already-absent, service-marked-for-delete, and power-loss-between-final-queue-and-`DeleteService` cases. The last case may leave only an inert SCM entry pointing at a deleted unique helper; prove it cannot run camera work and that an explicit fresh commissioning repairs package bytes and reconfigures that entry rather than reusing old identity.
- Fail immediately after the child helper readiness acknowledgement, during its post-parent marker check, and while acquiring its lifecycle lock. Also hold the service in `StopPending`. In every case prove the canonical service or transient helper remains a same-boot removal-only retry actor rather than silently parking until reboot.
- Exercise SCM service already absent and service marked-for-delete behavior as successful idempotent progress, while treating other SCM errors as failures.
- Interrupt after service stop and after executable deletion scheduling; reboot and prove convergence.
- Verify stale transient update/removal helpers are bounded and removed.
- Verify ProgramData identity cleanup, Gateway registry deletion, private-key/certificate cleanup, package cleanup, and FFmpeg child termination.
- Confirm native service failure/restart policy handles a post-commit start failure where the candidate can launch recovery code.

## Linux amd64 probes

Use `camos-gateway.service` on the production filesystem/service manager.

- Exercise active, inactive, disabled, and already-absent service states.
- Reboot during download, replacement, and removal finalization.
- Prove same-filesystem active replacement is atomic and post-rename sync/restart errors are classified post-commit.
- Prove the removal unit remains installed until all important canonical cleanup succeeds.
- Unlink the removal unit file while its unit remains loaded in systemd. Commissioning and normal startup must remain blocked until `daemon-reload` proves the cached unit absent; an ambiguous `LoadState` probe must fail closed.
- Prove the helper reduces sensitive state, removes GatewayID, and atomically renames `/var/lib/camos-gateway` to `/var/lib/camos-gateway.removing` while both flocks remain held. Reboot at that boundary and prove the stable removal unit deletes only the tombstone, then its own native state.
- Pause lifecycle waiters on the old directory and marker inodes across the rename. They must fail post-flock canonical-name validation and must never mutate a later commission.

## macOS Intel and Apple Silicon probes

Use LaunchDaemon `com.camos.gateway` on both amd64 and arm64 hardware.

- Exercise loaded, unloaded, disabled, and genuinely absent launchd states.
- Inject an ambiguous `launchctl` error and prove it is not interpreted as absence.
- Unlink the removal plist while its job remains loaded. Commissioning and normal startup must remain blocked until `launchctl print` proves the cached job absent.
- Reboot during download, replacement, and removal finalization.
- Prove same-filesystem active replacement is atomic and post-rename sync/restart errors are classified post-commit.
- Prove the helper reduces sensitive state, removes GatewayID, and atomically renames `/Library/Application Support/camOS Gateway` to `/Library/Application Support/camOS Gateway.removing` while both flocks remain held. Reboot at that boundary and prove the stable removal LaunchDaemon deletes only the tombstone, then removes itself.
- Pause lifecycle waiters on the old directory and marker inodes across the rename. They must fail post-flock canonical-name validation and must never mutate a later commission.

## Accepted residual boundary

There is intentionally no permanent launcher, updater service, watchdog, local database, or release-history framework. A replacement that passes static checks but is still too broken for the OS to launch any Gateway recovery code can require physical repair. One previous executable and native service supervision reduce risk but cannot eliminate this no-launch boundary.

Acceptance of that residual risk depends on all of the following evidence: exact metadata and SHA validation, static PE/ELF/Mach-O and Go identity checks, native builds, real Google verification, upgrade and downgrade tests, replacement/reboot fault injection, and four-target physical qualification.

Windows SCM service deletion and Session Manager delayed-file deletion do not share an atomic transaction. The implementation publishes the complete final delayed-delete queue before deleting the last SCM actor, choosing a possible inert service-name residue in the narrow intervening power-loss window over stranded credentials, a reusable canonical file deletion, or camera resurrection. The old package, GatewayID, credentials, marker, and helper still move to absence; fresh commissioning repairs the inert service entry. Physical acceptance must measure and explicitly accept or reject this cleanup-only boundary before commercial distribution.

Do not label the release commercially ready until every item above has dated evidence and an explicit production owner sign-off.
