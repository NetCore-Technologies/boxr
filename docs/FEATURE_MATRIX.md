# Implemented feature inventory

The CLI exposes this same inventory through `boxr features` and `boxr features --json`. This document is generated from the feature registry and checked by the CLI test suite.

**Inventory size: 651 capabilities.** Each entry maps to implemented behaviour, an explicit sub-control, or a testable tooling target.

## Implemented capabilities

001. help and usage
002. version metadata
003. structured version fields
004. JSON version output
005. read-only host doctor
006. JSON doctor output
007. strict doctor warning mode
008. run command with arguments
009. unknown command diagnostics
010. repeatable environment overrides
011. environment key validation
012. NUL-safe environment validation
013. duplicate environment key collapse
014. environment files with comments and export syntax
015. single and double quoted env values
016. hostname isolation
017. working directory
018. memory hard limit
019. memory high threshold
020. memory low protection
021. memory min protection
022. memory swap cap
023. memory OOM group
024. CPU quota
025. CPU weight
026. block I/O weight
027. PID count limit
028. cgroup v2 controller delegation checks
029. cgroup rollback on failed setup
030. cgroup cleanup after exit
031. namespace startup barrier
032. PID namespace init supervisor
033. child process reaping
034. signal forwarding
035. workload exit-code propagation
036. private mount propagation
037. optional root filesystem
038. chroot execution
039. read-only root filesystem
040. proc filesystem mount
041. recursive bind mounts
042. read-only bind mounts
043. tmpfs mounts
044. tmpfs size cap
045. tmpfs mode
046. private network namespace
047. loopback interface activation
048. host networking mode
049. user namespace UID mapping
050. user namespace GID mapping
051. capability name parser
052. drop selected Linux capabilities
053. drop all Linux capabilities
054. no-new-privileges
055. rootfs archive path traversal rejection
056. rootfs archive size limits
057. rootfs archive entry limits
058. rootfs safe symlink extraction
059. rootfs absolute symlink normalization
060. rootfs hard-link extraction
061. rootfs special-file rejection
062. rootfs atomic extraction
063. rootfs pack
064. rootfs inspect
065. rootfs verify
066. rootfs deterministic SHA-256 digest
067. rootfs digest verification
068. rootfs directory diff with added/modified/removed paths
069. rootfs security audit without following symlinks
070. rootfs audit detects escaping relative symlinks
071. rootfs audit detects setuid files
072. rootfs audit detects setgid files
073. rootfs audit detects world-writable paths
074. rootfs audit treats sticky world-writable directories as informational
075. rootfs audit detects special files
076. rootfs audit JSON report
077. rootfs audit strict failure mode
078. image audit by local tag
079. image audit reuses safe rootfs extraction
080. image audit JSON report
081. image audit strict failure mode
082. rootfs audit configurable entry limit
083. rootfs audit rejects non-positive entry limits
084. rootfs audit fails on limit rather than returning partial results
085. image audit configurable entry limit
086. versioned JSON run config
087. strict JSON config field validation
088. config size limit
089. config example generator
090. config name inheritance
091. Go source formatting target
092. race-enabled test target
093. go vet target
094. cross-platform CLI builds
095. Linux amd64 build
096. Linux arm64 build
097. Windows CLI build
098. macOS Intel CLI build
099. macOS Apple Silicon CLI build
100. named containers
101. collision-resistant 12-character container IDs
102. duplicate container-name protection
103. persistent JSON container records
104. private 0600 container state files
105. environment-secret redaction from state
106. container labels
107. created lifecycle state
108. running lifecycle state
109. exit status persistence
110. container create/start/exit/remove event journal
111. bounded event-log reader
112. recent event tail limit
113. active container listing
114. completed container history listing
115. container filter by ID prefix
116. container filter by name
117. container filter by status
118. container filter by label
119. container inspect JSON
120. completed-container removal
121. running-container removal protection
122. remove-all-completed workflow
123. automatic record removal after exit
124. age-based state pruning
125. live cgroup memory stats
126. live cgroup CPU stats
127. live cgroup PID stats
128. live cgroup I/O stats
129. container stats JSON output
130. host and runtime info report
131. host info JSON output
132. non-Linux stats limitation reporting
133. combined stdout and stderr log capture
134. private 0600 container log files
135. bounded container log reads
136. log tail by line count
137. live log following
138. container stop with graceful timeout
139. container kill with named signals
140. container wait for completion
141. signal-aware exit code persistence
142. log cleanup on container removal
143. named persistent volumes
144. volume create with metadata
145. volume labels
146. volume inspect JSON
147. volume list table and JSON
148. volume duplicate-name protection
149. volume path traversal rejection
150. volume directory permission hardening
151. volume metadata permission hardening
152. volume symlink path rejection
153. volume remove with data cleanup
154. volume prune with explicit force
155. active volume usage detection
156. run with named volume bind mounts
157. read-only named volume mount
158. named volume persistence across runs
159. volume list name and label filtering
160. volume prune label filtering
161. volume export as safe gzip tar archive
162. OverlayFS writable upper layer
163. persistent overlay upper and work directories
164. overlay path traversal and nesting validation
165. overlay read-only remount compatibility
166. overlay mount path option validation
167. separate writable layer per caller-selected directory
168. OverlayFS upper/work concurrency lock
169. canonical OverlayFS storage lock path
170. symlink-resistant OverlayFS lock file
171. private 0600 OverlayFS lock permissions
172. OverlayFS lock alias contention detection
173. empty OverlayFS workdir and mountpoint validation
174. local OCI image layout store
175. OCI image index schema validation
176. OCI image manifest descriptors
177. OCI image config descriptors
178. content-addressed SHA-256 blob storage
179. custom documented Boxr rootfs layer media type
180. rootfs import as image reference
181. strict image reference validation
182. image tag replacement without duplicate index entries
183. image listing in table format
184. image listing in JSON format
185. quiet image reference listing
186. image inspection with platform and layer metadata
187. image manifest blob integrity verification
188. image config blob integrity verification
189. image layer blob integrity verification
190. safe image unpack to new rootfs destination
191. image tag removal without deleting shared blobs
192. portable OCI layout archive save
193. portable OCI layout archive load
194. archive descriptor validation before import
195. image blob garbage collection dry run
196. image blob garbage collection with explicit force
197. private symlink-resistant image store
198. atomic OCI index updates
199. local image retagging without copying blobs
200. image alias removal preserves shared content
201. built-in Linux seccomp baseline profile
202. seccomp architecture guard in BPF filter
203. seccomp denylist for namespace and kernel administration syscalls
204. seccomp denylist for kernel module and BPF operations
205. seccomp denylist for process tracing and cross-process memory access
206. seccomp denylist for perf and userfaultfd
207. seccomp automatically enables no-new-privileges
208. seccomp profile name validation
209. seccomp filter generation tests
210. seccomp filter subprocess integration test
211. OCI uncompressed layer diff_id verification
212. image prune rejects unexpected blob filenames
213. image store root symlink rejection
214. streaming SHA-256 verification for image layers
215. bounded OCI blob sizes
216. streaming portable image save and load
217. full image graph validation before garbage collection
218. redacted container execution dry-run plan
219. dry-run validates complete runtime options
220. dry-run reports environment variable names only
221. dry-run redacts environment and label values
222. dry-run avoids creating container lifecycle records
223. explicit local image integrity verification command
224. image store unique blob count and byte accounting
225. image store reachable content byte accounting
226. image store reclaimable content byte accounting
227. full image store blob digest audit
228. OCI Distribution registry reference parser
229. Docker Hub default registry resolution
230. Docker Hub implicit library namespace resolution
231. custom registry host and port references
232. registry pull by tag
233. registry pull by immutable SHA-256 manifest digest
234. registry manifest-list platform selection
235. registry platform defaulting to Linux host architecture
236. OCI Distribution Bearer challenge handling
237. scoped registry access-token requests
238. registry username and password environment credentials
239. HTTPS-only token realms for HTTPS registries
240. registry manifest digest verification
241. registry image config blob digest verification
242. streaming registry rootfs layer digest verification
243. registry descriptor size verification
244. registry image config platform verification
245. registry rootfs diff_id verification
246. bounded registry manifest and config downloads
247. bounded streaming registry layer downloads
248. explicit unsupported registry layer media type errors
249. temporary registry layer cleanup
250. registry image import into local OCI layout
251. digest-pinned pull mapped to stable local tag
252. OCI Distribution image push by tag
253. registry image push rejects immutable digest targets
254. registry push requests pull and push token scope
255. registry Bearer token reuse during upload
256. registry blob mount before uploading content
257. OCI monolithic registry blob upload
258. registry relative upload Location resolution
259. registry upload Location same-origin enforcement
260. streamed large rootfs layer uploads from disk
261. registry upload request replay after authentication challenge
262. registry config and layer descriptor verification before upload
263. registry rootfs diff_id verification before upload
264. registry manifest publication after blobs
265. registry published manifest digest verification
266. verified local OCI bundle export for registry push
267. private stable temporary layer copy for registry export
268. registry image push CLI supports local and remote references
269. registry credentials restricted to trusted token hosts
270. registry host port validation
271. registry manifest response content type validation
272. registry image config media type validation
273. manifest-list descriptor media type and size validation
274. registry Content-Length validation for rootfs layer downloads
275. registry image push supports localhost test registries
276. registry cross-platform pulls preserve OS architecture and variant in the local OCI image config
277. registry auth file explicit path support
278. Docker config.json inline auth support
279. Podman auth.json inline auth support
280. base64 username-password registry auth decoding
281. Docker Hub legacy auth host alias matching
282. registry auth file symlink rejection
283. registry auth file regular-file validation
284. registry auth file size limit
285. registry auth file JSON validation
286. environment registry credentials precedence over auth files
287. REGISTRY_AUTH_FILE credential discovery
288. DOCKER_CONFIG credential discovery
289. default Docker and Podman credential file discovery
290. credential helper execution is explicitly disabled
291. registry push authfile flag
292. registry pull authfile flag
293. event query by action type
294. event query by exact container name
295. event query by container ID prefix
296. event query by relative time window
297. event query by RFC3339 timestamp
298. event filtering before result limiting
299. safe volume restore from gzip-tar archive
300. volume import uses rootfs archive path validation
301. volume import rejects escaping symlinks and duplicate paths
302. volume import stages extraction before publication
303. volume import atomically publishes completed data
304. volume import supports metadata labels
305. volume import cleans up failed extraction
306. portable volume export and import round trip
307. volume logical disk usage report
308. volume usage counts regular files and directories
309. volume usage counts symlink target lengths
310. volume usage never follows symlinks
311. volume usage reports special entries separately
312. volume usage exposes JSON and snapshot semantics
313. volume clone by portable archive snapshot
314. volume clone rejects a source in active use
315. volume clone restores into a distinct target volume
316. volume clone supports target metadata labels
317. volume clone cleans temporary archive after completion
318. volume clone uses hardened archive extraction
319. ps quiet ID-only output
320. ps newest-N selection across completed and active records
321. ps non-truncated command display
322. image list reference-prefix filtering
323. image list operating-system filtering
324. image list architecture filtering
325. image list AND-combined filters
326. image list rejects unsupported filters
327. state directory symlink rejection
328. state record symlink and file-type validation
329. event journal symlink rejection
330. container log symlink rejection on read and write
331. image audit verifies manifest descriptor graph
332. image audit verifies config descriptor digest
333. image audit verifies compressed layer digest
334. image audit verifies uncompressed rootfs diff ID
335. image audit snapshots verified layer under store lock
336. image audit rechecks snapshot digest before extraction
337. image audit reports image platform metadata
338. image audit reports manifest and config digests
339. image audit reports layer media type and digest
340. image audit combines integrity and filesystem findings in JSON
341. run directly from a verified local image tag
342. run image extraction is isolated in a private temporary directory
343. run image temporary rootfs is removed after command completion
344. run image dry-run verifies metadata without extracting the image
345. run image and explicit rootfs options are mutually exclusive
346. run image provenance is recorded as a container label
347. safe non-executing JSON image build recipe
348. image build strict required tag validation
349. image build configurable recipe path
350. image build context directory selection
351. image build explicit COPY files and directories
352. image build runtime environment metadata
353. image build entrypoint and command metadata
354. image build working-directory creation
355. image build labels and user metadata
356. image build JSON metadata output
357. image build quiet reference output
358. image build rejects unknown recipe fields and executable RUN steps
359. image build bounded .boxrignore file
360. image build .boxrignore comment and blank-line parsing
361. image build .boxrignore basename glob matching
362. image build .boxrignore nested path glob matching
363. image build .boxrignore directory subtree exclusion
364. image build refuses negated ignore patterns
365. image build rejects ignored explicit COPY sources
366. image build rejects symlinked ignore files
367. image build rejects oversized ignore files
368. image build creates parent directories for file COPY destinations
369. image build explicit Linux target platform selection
370. image build supports amd64 and arm64 target images
371. image build supports ARM v5 v6 and v7 variants
372. image build supports ARM64 v8 variant metadata
373. image build supports 386 ppc64le riscv64 and s390x metadata
374. image build rejects non-Linux and malformed platform declarations
375. image diff compares two verified local image rootfs trees
376. image diff reports added paths
377. image diff reports removed paths
378. image diff reports modified file contents and metadata
379. image diff does not follow rootfs symlinks
380. image diff supports JSON output
381. image diff stages image extraction in a private temporary directory
382. image diff removes staging files after comparison
383. image lock pins a tag to its immutable manifest digest
384. image lock records OS architecture and variant
385. image lock uses a versioned strict JSON schema
386. image lock file is written atomically
387. image lock file is private mode 0600
388. image lock rejects symlinked destinations and parents
389. image lock rejects non-regular and oversized files
390. image lock verification detects mutable tag drift
391. image lock rejects unknown JSON fields and trailing data
392. image lock supports JSON CLI output
393. image SBOM inventories the verified rootfs filesystem
394. image SBOM records the manifest digest and target platform
395. image SBOM records regular file sizes and permission modes
396. image SBOM generates per-file SHA-256 checksums
397. image SBOM records symlink targets without following links
398. image SBOM reports directories and symlinks separately
399. image SBOM deterministic path ordering
400. image SBOM configurable entry limit
401. image SBOM rejects non-positive entry limits
402. image SBOM fails rather than return a partial inventory
403. image SBOM private temporary extraction cleanup
404. rootfs archive normalizes entry modification timestamps
405. rootfs archive normalizes access and change timestamps
406. rootfs archive normalizes UID and GID metadata
407. rootfs archive omits host account names
408. rootfs archives are byte-identical for equivalent trees with different mtimes
409. named runtime security profiles
410. default security profile preserves explicit runtime settings
411. baseline security profile enforces no-new-privileges
412. baseline security profile enables the built-in seccomp filter
413. baseline security profile drops NET_RAW
414. restricted security profile disables container networking
415. restricted security profile enables a private user namespace
416. restricted security profile enforces no-new-privileges and seccomp
417. restricted security profile drops high-risk Linux capabilities
418. untrusted security profile requires an explicit root filesystem
419. untrusted security profile enforces read-only rootfs
420. security profiles apply after user flags so weaker flags cannot relax mandatory controls
421. security profile is recorded in container metadata and dry-run plans
422. strict JSON Schema for versioned run configuration
423. config schema command emits a JSON Schema document
424. config schema rejects unknown top-level properties
425. config schema describes security profile options
426. system storage usage summary
427. system storage usage includes verified image-store bytes
428. system storage usage includes named volume logical bytes
429. system storage usage includes container metadata and logs
430. system storage usage JSON output
431. system storage scanner does not follow symlink entries
432. system storage scanner rejects symlinked roots
433. system storage scanner treats absent directories as empty
434. named resource sizing profiles
435. micro resource profile defaults
436. small resource profile defaults
437. medium resource profile defaults
438. large resource profile defaults
439. resource profiles fill only unset memory limits
440. resource profiles fill only unset CPU quotas
441. resource profiles fill only unset PID limits
442. resource profiles fill CPU and I/O weights
443. resource profile preserves explicit config limits
444. resource profile preserves explicit command-line overrides
445. resource profile name is validated
446. resource profile appears in dry-run plans
447. image list filter by ARM variant
448. image list filter by digest prefix
449. image list filter by metadata label presence
450. image list filter by exact metadata label value
451. image list filters combine with AND semantics across platform and metadata
452. image list rejects empty metadata label filter keys
453. privacy-preserving support bundle generation
454. support bundle includes Boxr build and Go runtime metadata
455. support bundle includes diagnostic names and statuses without detail strings
456. support bundle excludes environment values and command arguments
457. support bundle excludes container logs, state files, configuration and filesystem paths
458. support bundle rejects symlink destinations and requires explicit overwrite confirmation
459. support bundle archives and reports a SHA-256 digest
460. support bundle archive and entries use private file permissions
461. support bundle includes host kernel release metadata
462. support bundle reports cgroup v2 availability
463. live container statistics watch mode
464. configurable container statistics sampling interval
465. bounded container statistics sampling count
466. timestamped JSON-lines container statistics output
467. statistics watch exits cleanly when its container stops
468. statistics watch rejects invalid intervals and counts
469. statistics watch preserves the existing one-shot JSON interface
470. statistics watch reports output and cgroup read errors
471. live container event streaming
472. event follow supports action and container filters
473. event follow supports configurable polling intervals
474. event follow supports bounded capture counts
475. event follow streams compact JSON Lines or table rows
476. event follow supports since-based historical catch-up
477. event follow handles event log truncation by resuming
478. event follow reports event source and output errors
479. wait for container startup readiness
480. wait for container process completion
481. configurable lifecycle wait polling interval
482. lifecycle wait enforces timeout for either condition
483. lifecycle wait returns structured JSON results
484. lifecycle wait reports containers that exit before becoming ready
485. lifecycle wait supports multiple container IDs
486. lifecycle wait preserves the default exit-code output
487. container prune supports non-mutating dry runs
488. prune dry run lists candidate container IDs and states
489. prune dry run respects age thresholds
490. prune dry run excludes running and never-started containers
491. prune supports machine-readable JSON planning output
492. prune dry run leaves container records and logs untouched
493. named volume prune supports non-mutating dry runs
494. volume prune previews unused volume names and labels
495. volume prune reports volumes protected by active containers
496. volume prune dry run respects name and label filters
497. volume prune supports machine-readable JSON plans
498. volume prune requires force for destructive cleanup
499. container inspect optionally reports state and log disk usage
500. inspect size report includes separate state and log byte counts
501. inspect size report includes total bytes
502. inspect size scanner treats an absent log as zero bytes
503. inspect size scanner rejects symlink and special-file targets
504. container stats supports human-readable table output
505. container stats watch supports live table rows
506. container stats table includes timestamp memory CPU and PID metrics
507. container stats preserves JSON as the default output format
508. volume du inventories every named volume
509. volume du aggregates file and directory counts
510. volume du aggregates logical bytes across volumes
511. volume du all mode supports name and label filters
512. volume du all mode supports JSON summary output
513. image du inventories every tagged image
514. image du reports per-tag rootfs layer bytes and platform
515. image du all mode supports existing image metadata filters
516. image du distinguishes referenced layer bytes from unique store bytes
517. image du all mode supports JSON output
518. image du preserves the existing unique-blob usage report
519. image prune supports structured per-blob JSON reports
520. image prune reports the size of each unreferenced blob
521. image prune aggregates reclaimable bytes before deletion
522. image prune dry-run preserves every unreferenced blob
523. image prune retains explicit force confirmation for deletion
524. standard Docker save archive import through image load
525. plain tar and gzip-compressed Docker archive support
526. bounded Docker archive extraction and strict path validation
527. Docker archive manifest and config size validation
528. Docker archive repository tag and platform validation
529. multi-layer Docker root filesystem application in layer order
530. Docker layer uncompressed SHA-256 diff_id verification
531. Docker layer whiteout deletion semantics
532. Docker opaque-directory whiteout semantics
533. Docker image environment entrypoint command working-directory user and label preservation
534. Docker archive multi-tag import with complete preflight validation
535. Docker layer safe symlink and hard-link handling with special-file rejection
536. Docker-save-compatible image archive export
537. plain uncompressed tar output compatible with Docker load
538. Docker image config filenames derived from SHA-256 config identities
539. Docker archive repositories tag map for legacy-compatible consumers
540. Docker export preserves rootfs diff IDs platform and runtime config
541. Docker image layer is rehashed while expanding it for export
542. format-selectable image save with the existing OCI format as default
543. multi-image Docker save export from a single command
544. combine tags with identical Docker image configurations into one manifest entry
545. deduplicate shared rootfs layers across Docker image exports
546. deterministic ordering of Docker manifest config and layer entries
547. enforce Docker export image-count and duplicate-reference limits
548. standard OCI gzip layer media type support
549. standard OCI uncompressed tar layer media type support
550. Docker distribution gzip layer media type support
551. Docker distribution uncompressed tar layer media type support
552. bounded multi-layer registry manifest validation
553. per-layer compressed registry blob digest verification
554. per-layer uncompressed rootfs diff_id verification
555. ordered multi-layer registry filesystem application
556. registry layer whiteout and opaque-directory semantics
557. versioned multi-service deployment manifests
558. strict bounded deployment manifest loading
559. deployment service and stack name validation
560. per-service command and runtime option validation
561. deployment dependency reference validation
562. duplicate and self-dependency rejection
563. deployment dependency cycle detection
564. deterministic topological deployment ordering
565. deployment plan service resource summaries
566. secret-redacted JSON deployment plans
567. deployment plan example and validation commands
568. local OCI image references in deployment services
569. stable stack-scoped service container names
570. reserved deployment identity labels
571. attached multi-service deployment supervisor
572. dependency-ordered service process startup
573. service running-state startup gates
574. fail-fast service exit handling
575. reverse-order graceful supervisor shutdown
576. stack-scoped deployment process status
577. reverse-dependency-order deployment shutdown
578. deployment stop timeout escalation
579. safe optional deployment record and log removal
580. deployment lifecycle dry-run planning
581. parallel startup of dependency-independent deployment services
582. deployment-wide unique registry image prefetch
583. deployment pull platform selection
584. deployment pull registry auth-file support
585. canonical local image reference resolution before service start
586. deployment pull validates cached image architecture and variant
587. image-backed service startup enforces the host Linux platform
588. deployment TCP readiness probes for loopback services
589. deployment HTTP readiness probes for loopback services
590. readiness targets reject non-loopback destinations and URL credentials
591. readiness probe intervals timeouts attempts and status codes are bounded
592. readiness retries honor supervisor cancellation and startup deadlines
593. secret-redacted readiness metadata in deployment plans
594. readiness validation accounts for mandatory network-isolating security profiles
595. dependent service startup waits for configured readiness probes
596. parallel service waves select readiness probes by service identity
597. deployment-wide wait for all services running
598. deployment-wide wait for all services stopped
599. bounded stack-state polling with configurable deadlines
600. secret-redacted JSON deployment wait results
601. non-executing image recipes support scratch and local OCI image bases
602. local image bases are integrity-verified before reuse
603. image base platforms must match requested Linux build targets
604. base filesystems are safely extracted before context COPY operations
605. base environment variables inherit with deterministic key-based overrides
606. base entrypoint command working directory user and labels inherit with build overrides
607. stack-wide service-prefixed deployment logs
608. deployment logs filter by service name
609. follow mode streams and drains all service logs
610. image build writes resolve in-root symlinks and reject host-root escapes
611. container-absolute COPY symlinks are normalized inside the staged rootfs
612. versioned deployment named persistent volume mounts
613. named volume names and mount targets validated before startup
614. deployment preflights every named volume before creating services
615. named volumes reuse Boxr's secure persistent volume store
616. volume archive exports never overwrite an existing destination
617. volume archive exports publish atomically from a same-directory staging file
618. volume export JSON includes SHA-256 integrity and filesystem manifest metadata
619. versioned deployment service profiles
620. validated lowercase service profile identifiers and duplicate rejection
621. unprofiled baseline services remain enabled by default
622. repeatable command-line service profile selection
623. additive activation of services matching any selected profile
624. profile-disabled services are omitted from deployment plans
625. full manifest validation occurs before profile filtering
626. unknown deployment profiles are rejected
627. profile-filtered dependency graphs reject inactive dependencies
628. deployment image prefetch and startup are limited to selected profiles
629. deterministic service profile inventory
630. profile inventory groups services by opt-in profile
631. always-enabled services appear separately from opt-in profiles
632. services assigned to multiple profiles appear in every matching group
633. secret-free JSON and text profile inventory output
634. per-service ordered environment file loading in deployments
635. deployment environment files override service-inherited values in declaration order
636. explicit service environment entries override environment file values
637. environment file path traversal and duplicate path rejection
638. deployment environment file count and path length bounds
639. secret-free environment file metadata in deployment plans
640. per-service startup timeout overrides the deployment-wide default
641. zero per-service startup timeout inherits the deployment-wide default
642. service startup timeout bounds are validated between 10 milliseconds and 10 minutes
643. secret-free deployment plans expose per-service startup timeout metadata
644. per-service startup timeouts govern process and readiness startup gates
645. per-service shutdown grace overrides the stack-wide deploy down timeout
646. zero per-service shutdown grace inherits the stack-wide command timeout
647. service shutdown timeout bounds are validated between 10 milliseconds and 10 minutes
648. per-service shutdown grace is persisted as reserved non-secret runtime metadata
649. attached deployment rollback and interrupt shutdown honor service-specific grace periods
650. deploy down applies each service-specific shutdown deadline independently
651. only services exceeding their own shutdown grace are escalated to SIGKILL
