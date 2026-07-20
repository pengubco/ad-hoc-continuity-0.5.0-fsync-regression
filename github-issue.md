## Title

fs: per-file fsync in sparse-aware copyFile causes 17x regression for many-files workloads

## Body

### Problem

Commit a424ba1 ("Linux: Make copyFile sparse aware") introduced `tgt.Sync()` at the end of the new `copyFile` implementation in `fs/copy_linux.go`. While the sparse-aware copy logic itself is valuable, the per-file fsync causes a severe performance regression when copying directories with many files.

In my use case, this caused significant container start latency when using Docker-style volumes (`VOLUME` directive in Dockerfile). The container runtime's `volumeCopyUp` path uses `fs.CopyDir` to copy image content into the volume mount at container start. When the image has thousands of files in the volume path, the per-file fsync serializes all I/O and adds tens of seconds (or minutes) of startup delay.

### Benchmark Results

50,000 files × 10 KiB each, ext4, cold page cache:

| Method | Time |
|---|---|
| Old behavior (io.Copy, no fsync) | 4.4s |
| New sparse-aware, **no fsync** | 4.8s |
| New sparse-aware, single sync at end | 4.9s |
| New sparse-aware, **per-file fsync (current)** | **1m 16s** |

The per-file fsync makes the copy **17x slower**. The sparse-aware logic itself (SEEK_DATA/SEEK_HOLE + copy_file_range) adds only ~10% overhead vs the old io.Copy path.

Full writeup and reproduction code: https://github.com/pengubco/ad-hoc-continuity-0.5.0-fsync-regression

### Impact

This affects any code path that uses `fs.CopyDir` or `fs.CopyFile` for directories with many files:

- **Volume copy-up** — containerd CRI plugin copies image content into volume mounts at container start (`volumeCopyUp`). Related: #8639 in containerd/containerd.
- **`nerdctl cp`** — copying files into/out of containers.
- **Snapshot operations** — any directory tree replication using `continuity/fs`.

### Why per-file fsync is unnecessary

The old `openAndCopyFile` (io.Copy) never called fsync. For the primary callers of `CopyDir`:

- Volume copy-up: if the machine crashes, the container never started — the runtime redoes the copy from the immutable image layer on restart.
- Image extraction: interrupted extractions are discarded and re-extracted from the content store.

None of these paths require per-file durability. The data source is always available for replay.

### Suggestion

Remove `tgt.Sync()` from `copyFile`. If durability before return is desired for some callers, a single `sync()` at the end of `CopyDir` provides equivalent crash safety with <1% overhead (vs 17x with per-file). Alternatively, make it opt-in via a `CopyDirOpt`.

### Environment

- Linux (ext4)
- Go 1.26
- `continuity` at commit a424ba1
- AWS EC2 with EBS volumes